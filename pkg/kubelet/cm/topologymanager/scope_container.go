/*
Copyright 2020 The Kubernetes Authors.

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

package topologymanager

import (
	"k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/admission"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
)

type containerScope struct {
	scope
}

// Ensure containerScope implements Scope interface
var _ Scope = &containerScope{}

// NewContainerScope returns a container scope.
func NewContainerScope(policy Policy) Scope {
	return &containerScope{
		scope{
			name:             containerTopologyScope,
			podTopologyHints: podTopologyHints{},
			policy:           policy,
			podMap:           containermap.NewContainerMap(),
		},
	}
}
// 获取provider可以分配的资源，然后合并，最终分配资源
func (s *containerScope) Admit(pod *v1.Pod) lifecycle.PodAdmitResult {
	// Exception - Policy : none
	if s.policy.Name() == PolicyNone {
		// 直接分配资源
		return s.admitPolicyNone(pod)
	}

	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		// 选出来最合适的
		bestHint, admit := s.calculateAffinity(pod, &container)
		klog.InfoS("Best TopologyHint", "bestHint", bestHint, "pod", klog.KObj(pod), "containerName", container.Name)

		if !admit {
			return admission.GetPodAdmitResult(&TopologyAffinityError{})
		}
		klog.InfoS("Topology Affinity", "bestHint", bestHint, "pod", klog.KObj(pod), "containerName", container.Name)
		// 存起来选出的最合适的
		s.setTopologyHints(string(pod.UID), container.Name, bestHint)

		err := s.allocateAlignedResources(pod, &container)
		if err != nil {
			return admission.GetPodAdmitResult(err)
		}
	}
	return admission.GetPodAdmitResult(nil)
}
// 从各个provider获取各种资源
func (s *containerScope) accumulateProvidersHints(pod *v1.Pod, container *v1.Container) []map[string][]TopologyHint {
	var providersHints []map[string][]TopologyHint

	for _, provider := range s.hintProviders {
		// Get the TopologyHints for a Container from a provider.
		hints := provider.GetTopologyHints(pod, container)
		providersHints = append(providersHints, hints)
		klog.InfoS("TopologyHints", "hints", hints, "pod", klog.KObj(pod), "containerName", container.Name)
	}
	return providersHints
}
// 从各个provider获取各种资源，然后选出来最好的TopologyHint
func (s *containerScope) calculateAffinity(pod *v1.Pod, container *v1.Container) (TopologyHint, bool) {
	providersHints := s.accumulateProvidersHints(pod, container)
	bestHint, admit := s.policy.Merge(providersHints)
	klog.InfoS("ContainerTopologyHint", "bestHint", bestHint)
	return bestHint, admit
}
