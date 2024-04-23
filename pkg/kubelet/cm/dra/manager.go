/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dra

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/klog/v2"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1alpha3"
	dra "k8s.io/kubernetes/pkg/kubelet/cm/dra/plugin"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// draManagerStateFileName is the file name where dra manager stores its state
const draManagerStateFileName = "dra_manager_state"

// ManagerImpl is the structure in charge of managing DRA resource Plugins.
type ManagerImpl struct {
	// cache contains cached claim info
	cache *claimInfoCache

	// KubeClient reference
	kubeClient clientset.Interface
}

// NewManagerImpl creates a new manager.
func NewManagerImpl(kubeClient clientset.Interface, stateFileDirectory string, nodeName types.NodeName) (*ManagerImpl, error) {
	klog.V(2).InfoS("Creating DRA manager")

	claimInfoCache, err := newClaimInfoCache(stateFileDirectory, draManagerStateFileName)
	if err != nil {
		return nil, fmt.Errorf("failed to create claimInfo cache: %+v", err)
	}

	manager := &ManagerImpl{
		cache:      claimInfoCache,
		kubeClient: kubeClient,
	}

	return manager, nil
}

// PrepareResources attempts to prepare all of the required resource
// plugin resources for the input container, issue NodePrepareResources rpc requests
// for each new resource requirement, process their responses and update the cached
// containerResources on success.
func (m *ManagerImpl) PrepareResources(pod *v1.Pod) (rerr error) {
	batches := make(map[string][]*drapb.Claim)
	resourceClaims := make(map[types.UID]*resourceapi.ResourceClaim)
	for i := range pod.Spec.ResourceClaims {
		podClaim := &pod.Spec.ResourceClaims[i]
		klog.V(3).InfoS("Processing resource", "podClaim", podClaim.Name, "pod", pod.Name)
		claimName, mustCheckOwner, err := resourceclaim.Name(pod, podClaim)
		if err != nil {
			return fmt.Errorf("prepare resource claim: %v", err)
		}

		if claimName == nil {
			// Nothing to do.
			continue
		}
		// Query claim object from the API server
		resourceClaim, err := m.kubeClient.ResourceV1alpha2().ResourceClaims(pod.Namespace).Get(
			context.TODO(),
			*claimName,
			metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to fetch ResourceClaim %s referenced by pod %s: %+v", *claimName, pod.Name, err)
		}

		if mustCheckOwner {
			if err = resourceclaim.IsForPod(pod, resourceClaim); err != nil {
				return err
			}
		}

		// Check if pod is in the ReservedFor for the claim
		if !resourceclaim.IsReservedForPod(pod, resourceClaim) {
			return fmt.Errorf("pod %s(%s) is not allowed to use resource claim %s(%s)",
				pod.Name, pod.UID, *claimName, resourceClaim.UID)
		}

		// If no container actually uses the claim, then we don't need
		// to prepare it.
		if !claimIsUsedByPod(podClaim, pod) {
			klog.V(5).InfoS("Skipping unused resource", "claim", claimName, "pod", pod.Name)
			continue
		}

		// Add a defer to make sure we remove references to this pod in the
		// claimInfo cache in cases where this function returns an error.
		defer func(claim *resourceapi.ResourceClaim) {
			if rerr != nil {
				m.cache.Lock()
				claimInfo, exists := m.cache.get(claim.Name, claim.Namespace)
				if exists {
					claimInfo.deletePodReference(pod.UID)
				}
				m.cache.Unlock()
			}
		}(resourceClaim)

		// Atomically perform some operations on the claimInfo cache.
		err = m.cache.withLock(func() error {
			// Get a reference to the claim info for this claim from the cache.
			// If there isn't one yet, then add it to the cache.
			claimInfo, exists := m.cache.get(resourceClaim.Name, resourceClaim.Namespace)
			if !exists {
				claimInfo = m.cache.add(newClaimInfoFromClaim(resourceClaim))
			}

			// Add a reference to the current pod in the claim info.
			// We delay checkpointing of this change until this call
			// returns successfully. It is OK to do this because we
			// will only return successfully from this call if the
			// checkpoint has succeeded. That means if the kubelet is
			// ever restarted before this checkpoint succeeds, the pod
			// whose resources are being prepared would never have
			// started, so it's OK (actually correct) to not include it
			// in the cache.
			claimInfo.addPodReference(pod.UID)

			// If this claim is already prepared, there is no need to prepare it again.
			if claimInfo.isPrepared() {
				return nil
			}

			// This saved claim will be used to update ClaimInfo cache
			// after NodePrepareResources GRPC succeeds
			resourceClaims[claimInfo.ClaimUID] = resourceClaim

			// Loop through all plugins and prepare for calling NodePrepareResources.
			for _, resourceHandle := range claimInfo.ResourceHandles {
				// If no DriverName is provided in the resourceHandle, we
				// use the DriverName from the status
				pluginName := claimInfo.DriverName
				if pluginName == "" {
					pluginName = claimInfo.DriverName
				}
				claim := &drapb.Claim{
					Namespace:      claimInfo.Namespace,
					Uid:            string(claimInfo.ClaimUID),
					Name:           claimInfo.ClaimName,
					ResourceHandle: resourceHandle.Data,
				}
				if resourceHandle.StructuredData != nil {
					claim.StructuredResourceHandle = []*resourceapi.StructuredResourceHandle{resourceHandle.StructuredData}
				}
				batches[pluginName] = append(batches[pluginName], claim)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("locked cache operation: %w", err)
		}
	}

	// Call NodePrepareResources for all claims in each batch.
	// If there is any error, processing gets aborted.
	// We could try to continue, but that would make the code more complex.
	for pluginName, claims := range batches {
		// Call NodePrepareResources RPC for all resource handles.
		client, err := dra.NewDRAPluginClient(pluginName)
		if err != nil {
			return fmt.Errorf("failed to get DRA Plugin client for plugin name %s: %v", pluginName, err)
		}
		response, err := client.NodePrepareResources(context.Background(), &drapb.NodePrepareResourcesRequest{Claims: claims})
		if err != nil {
			// General error unrelated to any particular claim.
			return fmt.Errorf("NodePrepareResources failed: %v", err)
		}
		for claimUID, result := range response.Claims {
			reqClaim := lookupClaimRequest(claims, claimUID)
			if reqClaim == nil {
				return fmt.Errorf("NodePrepareResources returned result for unknown claim UID %s", claimUID)
			}
			if result.GetError() != "" {
				return fmt.Errorf("NodePrepareResources failed for claim %s/%s: %s", reqClaim.Namespace, reqClaim.Name, result.Error)
			}

			claim := resourceClaims[types.UID(claimUID)]

			// Atomically perform some operations on the claimInfo cache.
			err := m.cache.withLock(func() error {
				// Add the prepared CDI devices to the claim info
				info, exists := m.cache.get(claim.Name, claim.Namespace)
				if !exists {
					return fmt.Errorf("unable to get claim info for claim %s in namespace %s", claim.Name, claim.Namespace)
				}
				if err := info.setCDIDevices(pluginName, result.GetCDIDevices()); err != nil {
					return fmt.Errorf("unable to add CDI devices for plugin %s of claim %s in namespace %s", pluginName, claim.Name, claim.Namespace)
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("locked cache operation: %w", err)
			}
		}

		unfinished := len(claims) - len(response.Claims)
		if unfinished != 0 {
			return fmt.Errorf("NodePrepareResources left out %d claims", unfinished)
		}
	}

	// Atomically perform some operations on the claimInfo cache.
	err := m.cache.withLock(func() error {
		// Mark all pod claims as prepared.
		for _, claim := range resourceClaims {
			info, exists := m.cache.get(claim.Name, claim.Namespace)
			if !exists {
				return fmt.Errorf("unable to get claim info for claim %s in namespace %s", claim.Name, claim.Namespace)
			}
			info.setPrepared()
		}

		// Checkpoint to ensure all prepared claims are tracked.
		if err := m.cache.syncToCheckpoint(); err != nil {
			return fmt.Errorf("failed to checkpoint claimInfo state: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("locked cache operation: %w", err)
	}

	return nil
}

func lookupClaimRequest(claims []*drapb.Claim, claimUID string) *drapb.Claim {
	for _, claim := range claims {
		if claim.Uid == claimUID {
			return claim
		}
	}
	return nil
}

func claimIsUsedByPod(podClaim *v1.PodResourceClaim, pod *v1.Pod) bool {
	if claimIsUsedByContainers(podClaim, pod.Spec.InitContainers) {
		return true
	}
	if claimIsUsedByContainers(podClaim, pod.Spec.Containers) {
		return true
	}
	return false
}

func claimIsUsedByContainers(podClaim *v1.PodResourceClaim, containers []v1.Container) bool {
	for i := range containers {
		if claimIsUsedByContainer(podClaim, &containers[i]) {
			return true
		}
	}
	return false
}

func claimIsUsedByContainer(podClaim *v1.PodResourceClaim, container *v1.Container) bool {
	for _, c := range container.Resources.Claims {
		if c.Name == podClaim.Name {
			return true
		}
	}
	return false
}

// GetResources gets a ContainerInfo object from the claimInfo cache.
// This information is used by the caller to update a container config.
func (m *ManagerImpl) GetResources(pod *v1.Pod, container *v1.Container) (*ContainerInfo, error) {
	annotations := []kubecontainer.Annotation{}
	cdiDevices := []kubecontainer.CDIDevice{}

	for i, podResourceClaim := range pod.Spec.ResourceClaims {
		claimName, _, err := resourceclaim.Name(pod, &pod.Spec.ResourceClaims[i])
		if err != nil {
			return nil, fmt.Errorf("list resource claims: %v", err)
		}
		// The claim name might be nil if no underlying resource claim
		// was generated for the referenced claim. There are valid use
		// cases when this might happen, so we simply skip it.
		if claimName == nil {
			continue
		}
		for _, claim := range container.Resources.Claims {
			if podResourceClaim.Name != claim.Name {
				continue
			}

			err := m.cache.withRLock(func() error {
				claimInfo, exists := m.cache.get(*claimName, pod.Namespace)
				if !exists {
					return fmt.Errorf("unable to get claim info for claim %s in namespace %s", *claimName, pod.Namespace)
				}

				claimAnnotations := claimInfo.annotationsAsList()
				klog.V(3).InfoS("Add resource annotations", "claim", *claimName, "annotations", claimAnnotations)
				annotations = append(annotations, claimAnnotations...)

				devices := claimInfo.cdiDevicesAsList()
				klog.V(3).InfoS("Add CDI devices", "claim", *claimName, "CD devices", devices)
				cdiDevices = append(cdiDevices, devices...)

				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("locked cache operation: %w", err)
			}
		}
	}

	return &ContainerInfo{Annotations: annotations, CDIDevices: cdiDevices}, nil
}

// UnprepareResources calls a plugin's NodeUnprepareResource API for each resource claim owned by a pod.
// This function is idempotent and may be called multiple times against the same pod.
// As such, calls to the underlying NodeUnprepareResource API are skipped for claims that have
// already been successfully unprepared.
func (m *ManagerImpl) UnprepareResources(pod *v1.Pod) error {
	batches := make(map[string][]*drapb.Claim)
	claimNames := make(map[types.UID]string)
	for i := range pod.Spec.ResourceClaims {
		claimName, _, err := resourceclaim.Name(pod, &pod.Spec.ResourceClaims[i])
		if err != nil {
			return fmt.Errorf("unprepare resource claim: %v", err)
		}

		// The claim name might be nil if no underlying resource claim
		// was generated for the referenced claim. There are valid use
		// cases when this might happen, so we simply skip it.
		if claimName == nil {
			continue
		}

		// Atomically perform some operations on the claimInfo cache.
		err = m.cache.withLock(func() error {
			// Get the claim info from the cache
			claimInfo, exists := m.cache.get(*claimName, pod.Namespace)

			// Skip calling NodeUnprepareResource if claim info is not cached
			if !exists {
				return nil
			}

			// Skip calling NodeUnprepareResource if other pods are still referencing it
			if len(claimInfo.PodUIDs) > 1 {
				// We delay checkpointing of this change until
				// UnprepareResources returns successfully. It is OK to do
				// this because we will only return successfully from this call
				// if the checkpoint has succeeded. That means if the kubelet
				// is ever restarted before this checkpoint succeeds, we will
				// simply call into this (idempotent) function again.
				claimInfo.deletePodReference(pod.UID)
				return nil
			}

			// This claimInfo name will be used to update ClaimInfo cache
			// after NodeUnprepareResources GRPC succeeds
			claimNames[claimInfo.ClaimUID] = claimInfo.ClaimName

			// Loop through all plugins and prepare for calling NodeUnprepareResources.
			for _, resourceHandle := range claimInfo.ResourceHandles {
				// If no DriverName is provided in the resourceHandle, we
				// use the DriverName from the status
				pluginName := resourceHandle.DriverName
				if pluginName == "" {
					pluginName = claimInfo.DriverName
				}

				claim := &drapb.Claim{
					Namespace:      claimInfo.Namespace,
					Uid:            string(claimInfo.ClaimUID),
					Name:           claimInfo.ClaimName,
					ResourceHandle: resourceHandle.Data,
				}
				if resourceHandle.StructuredData != nil {
					claim.StructuredResourceHandle = []*resourceapi.StructuredResourceHandle{resourceHandle.StructuredData}
				}
				batches[pluginName] = append(batches[pluginName], claim)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("locked cache operation: %w", err)
		}
	}

	// Call NodeUnprepareResources for all claims in each batch.
	// If there is any error, processing gets aborted.
	// We could try to continue, but that would make the code more complex.
	for pluginName, claims := range batches {
		// Call NodeUnprepareResources RPC for all resource handles.
		client, err := dra.NewDRAPluginClient(pluginName)
		if err != nil {
			return fmt.Errorf("failed to get DRA Plugin client for plugin name %s: %v", pluginName, err)
		}
		response, err := client.NodeUnprepareResources(context.Background(), &drapb.NodeUnprepareResourcesRequest{Claims: claims})
		if err != nil {
			// General error unrelated to any particular claim.
			return fmt.Errorf("NodeUnprepareResources failed: %v", err)
		}

		for claimUID, result := range response.Claims {
			reqClaim := lookupClaimRequest(claims, claimUID)
			if reqClaim == nil {
				return fmt.Errorf("NodeUnprepareResources returned result for unknown claim UID %s", claimUID)
			}
			if result.GetError() != "" {
				return fmt.Errorf("NodeUnprepareResources failed for claim %s/%s: %s", reqClaim.Namespace, reqClaim.Name, result.Error)
			}

			claimName := claimNames[types.UID(claimUID)]

			// Atomically perform some operations on the claimInfo cache.
			err := m.cache.withLock(func() error {
				// Delete claim info from the cache only when unprepare succeeds.
				// This ensures that the status manager doesn't enter termination status
				// for the pod. This logic is implemented in
				// m.PodMightNeedToUnprepareResources and claimInfo.hasPodReference.
				m.cache.delete(claimName, pod.Namespace)
				return nil
			})
			if err != nil {
				return fmt.Errorf("locked cache operation: %w", err)
			}
		}

		unfinished := len(claims) - len(response.Claims)
		if unfinished != 0 {
			return fmt.Errorf("NodeUnprepareResources left out %d claims", unfinished)
		}
	}

	// Atomically perform some operations on the claimInfo cache.
	err := m.cache.withRLock(func() error {
		if err := m.cache.syncToCheckpoint(); err != nil {
			return fmt.Errorf("failed to checkpoint claimInfo state: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("locked cache operation: %w", err)
	}

	return nil
}

// PodMightNeedToUnprepareResources returns true if the pod might need to
// unprepare resources
func (m *ManagerImpl) PodMightNeedToUnprepareResources(UID types.UID) bool {
	m.cache.Lock()
	defer m.cache.Unlock()
	return m.cache.hasPodReference(UID)
}

// GetContainerClaimInfos gets Container's ClaimInfo
func (m *ManagerImpl) GetContainerClaimInfos(pod *v1.Pod, container *v1.Container) ([]*ClaimInfo, error) {
	claimInfos := make([]*ClaimInfo, 0, len(pod.Spec.ResourceClaims))

	for i, podResourceClaim := range pod.Spec.ResourceClaims {
		claimName, _, err := resourceclaim.Name(pod, &pod.Spec.ResourceClaims[i])
		if err != nil {
			return nil, fmt.Errorf("determine resource claim information: %v", err)
		}

		for _, claim := range container.Resources.Claims {
			if podResourceClaim.Name != claim.Name {
				continue
			}

			err := m.cache.withRLock(func() error {
				claimInfo, exists := m.cache.get(*claimName, pod.Namespace)
				if !exists {
					return fmt.Errorf("unable to get claim info for claim %s in namespace %s", *claimName, pod.Namespace)
				}
				claimInfos = append(claimInfos, claimInfo.DeepCopy())
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("locked cache operation: %w", err)
			}
		}
	}
	return claimInfos, nil
}
