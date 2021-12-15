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

package checkpoint

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
)

// DeviceManagerCheckpoint defines the operations to retrieve pod devices
type DeviceManagerCheckpoint interface {
	checkpointmanager.Checkpoint
	GetData() ([]PodDevicesEntry, map[string][]string)
}

// DevicesPerNUMA represents device ids obtained from device plugin per NUMA node id
// key是numa的id，value是每个node包含的设备id
type DevicesPerNUMA map[int64][]string

// PodDevicesEntry connects pod information to devices
// pod和Device的连接信息，一个这个结构一般代表一个容器的分配单位
type PodDevicesEntry struct {
	PodUID        string
	ContainerName string
	ResourceName  string
	DeviceIDs     DevicesPerNUMA
	AllocResp     []byte
}

// checkpointData struct is used to store pod to device allocation information
// in a checkpoint file.
// TODO: add version control when we need to change checkpoint format.
type checkpointData struct {
	PodDeviceEntries  []PodDevicesEntry  // 每个item是一个pod分配的资源
	RegisteredDevices map[string][]string  // 所有注册的设备
}

// Data holds checkpoint data and its checksum
type Data struct {
	Data     checkpointData
	Checksum checksum.Checksum
}

// NewDevicesPerNUMA is a function that creates DevicesPerNUMA map
func NewDevicesPerNUMA() DevicesPerNUMA {
	return make(DevicesPerNUMA)
}

// Devices is a function that returns all device ids for all NUMA nodes
// and represent it as sets.String
// 所有NUMA节点的所有deviceid
func (dev DevicesPerNUMA) Devices() sets.String {
	result := sets.NewString()

	for _, devs := range dev {
		result.Insert(devs...)
	}
	return result
}

// New returns an instance of Checkpoint
func New(devEntries []PodDevicesEntry,
	devices map[string][]string) DeviceManagerCheckpoint {
	return &Data{
		Data: checkpointData{
			PodDeviceEntries:  devEntries,
			RegisteredDevices: devices,
		},
	}
}

// MarshalCheckpoint returns marshalled data
func (cp *Data) MarshalCheckpoint() ([]byte, error) {
	cp.Checksum = checksum.New(cp.Data)
	return json.Marshal(*cp)
}

// UnmarshalCheckpoint returns unmarshalled data
func (cp *Data) UnmarshalCheckpoint(blob []byte) error {
	return json.Unmarshal(blob, cp)
}

// VerifyChecksum verifies that passed checksum is same as calculated checksum
func (cp *Data) VerifyChecksum() error {
	return cp.Checksum.Verify(cp.Data)
}

// GetData returns device entries and registered devices
func (cp *Data) GetData() ([]PodDevicesEntry, map[string][]string) {
	return cp.Data.PodDeviceEntries, cp.Data.RegisteredDevices
}
