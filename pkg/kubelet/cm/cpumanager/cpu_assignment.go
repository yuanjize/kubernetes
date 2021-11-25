/*
Copyright 2017 The Kubernetes Authors.

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

package cpumanager

import (
	"fmt"
	"sort"

	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)
// 分配cpu的时候尽可能近的分配，比如都在一个socket/core上，这样运行效率比较高
type cpuAccumulator struct {
	topo          *topology.CPUTopology // 节点cpu信息
	details       topology.CPUDetails  // 当前未分配给我的的cpu info集合
	numCPUsNeeded int   //当前我还需要多少cpu
	result        cpuset.CPUSet  // 已经分配给我的cpu
}

func newCPUAccumulator(topo *topology.CPUTopology, availableCPUs cpuset.CPUSet, numCPUs int) *cpuAccumulator {
	return &cpuAccumulator{
		topo:          topo,
		details:       topo.CPUDetails.KeepOnly(availableCPUs),
		numCPUsNeeded: numCPUs,
		result:        cpuset.NewCPUSet(),
	}
}

// Returns true if the supplied socket is fully available in `topoDetails`.
// 套接字是否可用
func (a *cpuAccumulator) isSocketFree(socketID int) bool {
	return a.details.CPUsInSockets(socketID).Size() == a.topo.CPUsPerSocket()
}

// Returns true if the supplied core is fully available in `topoDetails`.
// core是否可用（物理cpu）
func (a *cpuAccumulator) isCoreFree(coreID int) bool {
	return a.details.CPUsInCores(coreID).Size() == a.topo.CPUsPerCore()
}

// Returns free socket IDs as a slice sorted by sortAvailableSockets().
// 获取所有free sockets
func (a *cpuAccumulator) freeSockets() []int {
	free := []int{}
	for _, socket := range a.sortAvailableSockets() {
		if a.isSocketFree(socket) {
			free = append(free, socket)
		}
	}
	return free
}

// Returns free core IDs as a slice sorted by sortAvailableCores().
// 发挥所有free的core
func (a *cpuAccumulator) freeCores() []int {
	free := []int{}
	for _, core := range a.sortAvailableCores() {
		if a.isCoreFree(core) {
			free = append(free, core)
		}
	}
	return free
}

// Returns free CPU IDs as a slice sorted by sortAvailableCPUs().
// 返回所有的逻辑cpu
func (a *cpuAccumulator) freeCPUs() []int {
	return a.sortAvailableCPUs()
}

// Sorts the provided list of sockets/cores/cpus referenced in 'ids' by the
// number of available CPUs contained within them (smallest to largest). The
// 'getCPU()' paramater defines the function that should be called to retrieve
// the list of available CPUs for the type of socket/core/cpu being referenced.
// If two sockets/cores/cpus have the same number of available CPUs, they are
// sorted in ascending order by their id.
func (a *cpuAccumulator) sort(ids []int, getCPUs func(ids ...int) cpuset.CPUSet) {
	sort.Slice(ids,
		func(i, j int) bool {
			iCPUs := getCPUs(ids[i])
			jCPUs := getCPUs(ids[j])
			if iCPUs.Size() < jCPUs.Size() {
				return true
			}
			if iCPUs.Size() > jCPUs.Size() {
				return false
			}
			return ids[i] < ids[j]
		})
}

// Sort all sockets with free CPUs using the sort() algorithm defined above.
// 根据sockets包含的core个数来排序
func (a *cpuAccumulator) sortAvailableSockets() []int {
	sockets := a.details.Sockets().ToSliceNoSort()
	a.sort(sockets, a.details.CPUsInSockets)
	return sockets
}

// Sort all cores with free CPUs:
// - First by socket using sortAvailableSockets().
// - Then within each socket, using the sort() algorithm defined above.
// 首先根据sockets包含的cpu个数来排序，然后根据core包含的cpu数来排序，最后返回所有core
func (a *cpuAccumulator) sortAvailableCores() []int {
	var result []int
	for _, socket := range a.sortAvailableSockets() {
		cores := a.details.CoresInSockets(socket).ToSliceNoSort()
		a.sort(cores, a.details.CPUsInCores)
		result = append(result, cores...)
	}
	return result
}

// Sort all available CPUs:
// - First by core using sortAvailableCores().
// - Then within each core, using the sort() algorithm defined above.
// 首先 sortAvailableCores 排序，然后根据核心包含的cpu数来排序
func (a *cpuAccumulator) sortAvailableCPUs() []int {
	var result []int
	for _, core := range a.sortAvailableCores() {
		cpus := a.details.CPUsInCores(core).ToSliceNoSort()
		sort.Ints(cpus)
		result = append(result, cpus...)
	}
	return result
}
// 分配cpus给我
func (a *cpuAccumulator) take(cpus cpuset.CPUSet) {
	a.result = a.result.Union(cpus)
	a.details = a.details.KeepOnly(a.details.CPUs().Difference(a.result)) // 更新未分配cpu
	a.numCPUsNeeded -= cpus.Size()  // 需要的cpu数减少
}
// 拿走整个sockets的cpu
func (a *cpuAccumulator) takeFullSockets() {
	for _, socket := range a.freeSockets() {
		cpusInSocket := a.topo.CPUDetails.CPUsInSockets(socket)
		if !a.needs(cpusInSocket.Size()) {
			continue
		}
		klog.V(4).InfoS("takeFullSockets: claiming socket", "socket", socket)
		a.take(cpusInSocket)
	}
}
// 拿走整个核心的cpu
func (a *cpuAccumulator) takeFullCores() {
	for _, core := range a.freeCores() {
		cpusInCore := a.topo.CPUDetails.CPUsInCores(core)
		if !a.needs(cpusInCore.Size()) {
			continue
		}
		klog.V(4).InfoS("takeFullCores: claiming core", "core", core)
		a.take(cpusInCore)
	}
}
// 一个一个cpu的拿
func (a *cpuAccumulator) takeRemainingCPUs() {
	for _, cpu := range a.sortAvailableCPUs() {
		klog.V(4).InfoS("takeRemainingCPUs: claiming CPU", "cpu", cpu)
		a.take(cpuset.NewCPUSet(cpu))
		if a.isSatisfied() {
			return
		}
	}
}

func (a *cpuAccumulator) needs(n int) bool {
	return a.numCPUsNeeded >= n
}
// 是否不再需要cpu
func (a *cpuAccumulator) isSatisfied() bool {
	return a.numCPUsNeeded < 1
}
// 需要的cpu大于库存
func (a *cpuAccumulator) isFailed() bool {
	return a.numCPUsNeeded > a.details.CPUs().Size()
}

// 尽力高效率的分配cpu(thread)，真正干活儿的
func takeByTopology(topo *topology.CPUTopology, availableCPUs cpuset.CPUSet, numCPUs int) (cpuset.CPUSet, error) {
	acc := newCPUAccumulator(topo, availableCPUs, numCPUs)
	// 已经满足cpu分配，直接返回
	if acc.isSatisfied() {
		return acc.result, nil
	}
	// 永远满足不了cpu分配
	if acc.isFailed() {
		return cpuset.NewCPUSet(), fmt.Errorf("not enough cpus available to satisfy request")
	}

	// Algorithm: topology-aware best-fit
	// 1. Acquire whole sockets, if available and the container requires at
	//    least a socket's-worth of CPUs.
	// 如果一个socket上的所有cpu可以满足需求，那么就分配（同一个socket的cpu运行效率高）
	acc.takeFullSockets()
	if acc.isSatisfied() {
		return acc.result, nil
	}

	// 2. Acquire whole cores, if available and the container requires at least
	//    a core's-worth of CPUs.
	// 1个socket上的所有cpu不满足需求，那么接着要一个core上的所有cpu
	acc.takeFullCores()
	if acc.isSatisfied() {
		return acc.result, nil
	}

	// 3. Acquire single threads, preferring to fill partially-allocated cores
	//    on the same sockets as the whole cores we have already taken in this
	//    allocation.
	// 最后只能激励分配单个cpu，因为这里用了排序算法，所以分配的多个cpu会尽可能的靠近
	acc.takeRemainingCPUs()
	if acc.isSatisfied() {
		return acc.result, nil
	}

	return cpuset.NewCPUSet(), fmt.Errorf("failed to allocate cpus")
}
