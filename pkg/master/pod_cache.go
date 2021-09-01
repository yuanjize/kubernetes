/*
Copyright 2014 Google Inc. All rights reserved.

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

package master

import (
	"sync"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/pod"

	"github.com/golang/glog"
)

// PodCache contains both a cache of container information, as well as the mechanism for keeping
// that cache up to date.
type PodCache struct {
	containerInfo client.PodInfoGetter  //一个http api用来获取podinfo
	pods          pod.Registry  // 用来获取pods的api接口
	// This is a map of pod id to a map of container name to the
	podInfo map[string]api.PodInfo  // pod缓存
	podLock sync.Mutex
}

// NewPodCache returns a new PodCache which watches container information registered in the given PodRegistry.
func NewPodCache(info client.PodInfoGetter, pods pod.Registry) *PodCache {
	return &PodCache{
		containerInfo: info,
		pods:          pods,
		podInfo:       map[string]api.PodInfo{},
	}
}
// 通过Cache获取PodInfo
// GetPodInfo implements the PodInfoGetter.GetPodInfo.
// The returned value should be treated as read-only.
// TODO: Remove the host from this call, it's totally unnecessary.
func (p *PodCache) GetPodInfo(host, podID string) (api.PodInfo, error) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	value, ok := p.podInfo[podID]
	if !ok {
		return nil, client.ErrPodInfoNotAvailable
	}
	return value, nil
}
// 用HTTP API取得Pod 放到cache中
func (p *PodCache) updatePodInfo(host, id string) error {
	info, err := p.containerInfo.GetPodInfo(host, id)
	if err != nil {
		return err
	}
	p.podLock.Lock()
	defer p.podLock.Unlock()
	p.podInfo[id] = info
	return nil
}
// 从ETCD中取得所有的pod的host和ip，然后用http api取得PodInfo
// UpdateAllContainers updates information about all containers.  Either called by Loop() below, or one-off.
func (p *PodCache) UpdateAllContainers() {
	pods, err := p.pods.ListPods(labels.Everything())
	if err != nil {
		glog.Errorf("Error synchronizing container list: %v", err)
		return
	}
	for _, pod := range pods.Items {
		err := p.updatePodInfo(pod.CurrentState.Host, pod.ID)
		if err != nil && err != client.ErrPodInfoNotAvailable {
			glog.Errorf("Error synchronizing container: %v", err)
		}
	}
}
