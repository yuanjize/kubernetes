/*
Copyright 2019 The Kubernetes Authors.

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
// 尽力选出最好的hint
type bestEffortPolicy struct {
	//List of NUMA Nodes available on the underlying machine
	numaNodes []int // 底层机器可以用的numa颗数
}

var _ Policy = &bestEffortPolicy{}

// PolicyBestEffort policy name.
const PolicyBestEffort string = "best-effort"

// NewBestEffortPolicy returns best-effort policy.
func NewBestEffortPolicy(numaNodes []int) Policy {
	return &bestEffortPolicy{numaNodes: numaNodes}
}

func (p *bestEffortPolicy) Name() string {
	return PolicyBestEffort
}
// 是否允许使用该hint
func (p *bestEffortPolicy) canAdmitPodResult(hint *TopologyHint) bool {
	return true
}

func (p *bestEffortPolicy) Merge(providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
	// flat hints
	filteredProvidersHints := filterProvidersHints(providersHints)
	// 选出最佳的 hints
	bestHint := mergeFilteredHints(p.numaNodes, filteredProvidersHints)
	// true
	admit := p.canAdmitPodResult(&bestHint)
	return bestHint, admit
}
