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

package state

import (
	v1 "k8s.io/api/core/v1"
)

// MemoryTable contains memory information
// 一个numa node的内存信息
type MemoryTable struct {
	TotalMemSize   uint64 `json:"total"`          // 总内存大小(hugepage的话就是可用的总的hugepage)
	SystemReserved uint64 `json:"systemReserved"` // 保留内存大小，给系统守护进程和kubelete用
	Allocatable    uint64 `json:"allocatable"`    //可分配内存大小
	Reserved       uint64 `json:"reserved"`      // 目前认为是已分配内存大小
	Free           uint64 `json:"free"` // 可以使用的内存大小，初始化 Allocatable=Free=TotalMemSize-SystemReserved
}

// NUMANodeState contains NUMA node related information
// 每个numa节点的内存使用状态
type NUMANodeState struct {
	// NumberOfAssignments contains a number memory assignments from this node
	// When the container requires memory and hugepages it will increase number of assignments by two
	// 有几个容器的block使用了该node的内存
	NumberOfAssignments int `json:"numberOfAssignments"`
	// MemoryTable contains NUMA node memory related information
	// numa节点内存信息
	MemoryMap map[v1.ResourceName]*MemoryTable `json:"memoryMap"`
	// Cells contains the current NUMA node and all other nodes that are in a group with current NUMA node
	// This parameter indicates if the current node is used for the multiple NUMA node memory allocation
	// For example if some container has pinning 0,1,2, NUMA nodes 0,1,2 under the state will have
	// this parameter equals to [0, 1, 2]
	Cells []int `json:"cells"`
}

// NUMANodeMap contains memory information for each NUMA node.
// NUMA内存节点状态
type NUMANodeMap map[int]*NUMANodeState

// Clone returns a copy of NUMANodeMap
func (nm NUMANodeMap) Clone() NUMANodeMap {
	clone := make(NUMANodeMap)
	for node, s := range nm {
		if s == nil {
			clone[node] = nil
			continue
		}

		clone[node] = &NUMANodeState{}
		clone[node].NumberOfAssignments = s.NumberOfAssignments
		clone[node].Cells = append([]int{}, s.Cells...)

		if s.MemoryMap == nil {
			continue
		}

		clone[node].MemoryMap = map[v1.ResourceName]*MemoryTable{}
		for memoryType, memoryTable := range s.MemoryMap {
			clone[node].MemoryMap[memoryType] = &MemoryTable{
				Allocatable:    memoryTable.Allocatable,
				Free:           memoryTable.Free,
				Reserved:       memoryTable.Reserved,
				SystemReserved: memoryTable.SystemReserved,
				TotalMemSize:   memoryTable.TotalMemSize,
			}
		}
	}
	return clone
}

// Block is a data structure used to represent a certain amount of memory
type Block struct {
	// NUMAAffinity contains the string that represents NUMA affinity bitmask
	NUMAAffinity []int           `json:"numaAffinity"`
	Type         v1.ResourceName `json:"type"`
	Size         uint64          `json:"size"`
}

// ContainerMemoryAssignments stores memory assignments of containers
// 记录给容器分配的内存
type ContainerMemoryAssignments map[string]map[string][]Block

// Clone returns a copy of ContainerMemoryAssignments
func (as ContainerMemoryAssignments) Clone() ContainerMemoryAssignments {
	clone := make(ContainerMemoryAssignments)
	for pod := range as {
		clone[pod] = make(map[string][]Block)
		for container, blocks := range as[pod] {
			clone[pod][container] = append([]Block{}, blocks...)
		}
	}
	return clone
}

// Reader interface used to read current memory/pod assignment state
type Reader interface {
	// GetMachineState returns Memory Map stored in the State
	GetMachineState() NUMANodeMap
	// GetMemoryBlocks returns memory assignments of a container
	GetMemoryBlocks(podUID string, containerName string) []Block
	// GetMemoryAssignments returns ContainerMemoryAssignments
	GetMemoryAssignments() ContainerMemoryAssignments
}

type writer interface {
	// SetMachineState stores NUMANodeMap in State
	SetMachineState(memoryMap NUMANodeMap)
	// SetMemoryBlocks stores memory assignments of a container
	SetMemoryBlocks(podUID string, containerName string, blocks []Block)
	// SetMemoryAssignments sets ContainerMemoryAssignments by using the passed parameter
	SetMemoryAssignments(assignments ContainerMemoryAssignments)
	// Delete deletes corresponding Blocks from ContainerMemoryAssignments
	Delete(podUID string, containerName string)
	// ClearState clears machineState and ContainerMemoryAssignments
	ClearState()
}

// State interface provides methods for tracking and setting memory/pod assignment
type State interface {
	Reader
	writer
}
