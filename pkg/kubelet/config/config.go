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

package config

import (
	"fmt"
	"reflect"
	"sync"

	apierrs "github.com/GoogleCloudPlatform/kubernetes/pkg/api/errors"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/config"
	"github.com/golang/glog"
)

// PodConfigNotificationMode describes how changes are sent to the update channel.
type PodConfigNotificationMode int

const (
	// PodConfigNotificationSnapshot delivers the full configuration as a SET whenever
	// any change occurs.  发送当前的一个快照
	PodConfigNotificationSnapshot = iota
	// PodConfigNotificationSnapshotAndUpdates delivers an UPDATE message whenever pods are
	// changed, and a SET message if there are any additions or removals. update单独通知，其他的用一个当前pods的快照当作一个消息通知
	PodConfigNotificationSnapshotAndUpdates
	// PodConfigNotificationIncremental delivers ADD, UPDATE, and REMOVE to the update channel. 把add，delete，update分别通知到channel
	PodConfigNotificationIncremental
)
/*
就是一个配置，提供两个对外的接口，一个是给外部让它写，一个是用来给外部来监听更新。它自己本身也是一个内存cache
*/
// PodConfig is a configuration mux that merges many sources of pod configuration into a single
// consistent structure, and then delivers incremental change notifications to listeners
// in order.
type PodConfig struct {
	pods *podStorage
	mux  *config.Mux

	// the channel of denormalized changes passed to listeners
	updates chan kubelet.PodUpdate
}

// NewPodConfig creates an object that can merge many configuration sources into a stream
// of normalized updates to a pod configuration.
func NewPodConfig(mode PodConfigNotificationMode) *PodConfig {
	updates := make(chan kubelet.PodUpdate, 1)
	storage := newPodStorage(updates, mode)
	podConfig := &PodConfig{
		pods:    storage,
		mux:     config.NewMux(storage),
		updates: updates,
	}
	return podConfig
}
// 外部写数据用这个接口
// Channel creates or returns a config source channel.  The channel
// only accepts PodUpdates
func (c *PodConfig) Channel(source string) chan<- interface{} {
	return c.mux.Channel(source)
}
// 外部监听更新数据用这个借口
// Updates returns a channel of updates to the configuration, properly denormalized.
func (c *PodConfig) Updates() <-chan kubelet.PodUpdate {
	return c.updates
}

// Sync requests the full configuration be delivered to the update channel.
func (c *PodConfig) Sync() {
	c.pods.Sync()
}

// podStorage manages the current pod state at any point in time and ensures updates
// to the channel are delivered in order.  Note that this object is an in-memory source of
// "truth" and on creation contains zero entries.  Once all previously read sources are
// available, then this object should be considered authoritative.
type podStorage struct {
	podLock sync.RWMutex
	// map of source name to pod name to pod reference
	pods map[string]map[string]*kubelet.Pod  // 可以认为是pod内存 cache
	mode PodConfigNotificationMode

	// ensures that updates are delivered in strict order
	// on the updates channel
	updateLock sync.Mutex
	updates    chan<- kubelet.PodUpdate
}

// TODO: PodConfigNotificationMode could be handled by a listener to the updates channel
// in the future, especially with multiple listeners.
// TODO: allow initialization of the current state of the store with snapshotted version.
// 可以认为是一个pod的cache，会监听pod的增删改查然后改变cache并回调一些listener
func newPodStorage(updates chan<- kubelet.PodUpdate, mode PodConfigNotificationMode) *podStorage {
	return &podStorage{
		pods:    make(map[string]map[string]*kubelet.Pod),
		mode:    mode,
		updates: updates,
	}
}
// merge pods，然后发送快照
// Merge normalizes a set of incoming changes from different sources into a map of all Pods
// and ensures that redundant changes are filtered out, and then pushes zero or more minimal
// updates onto the update channel.  Ensures that updates are delivered in order.
func (s *podStorage) Merge(source string, change interface{}) error {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()

	adds, updates, deletes := s.merge(source, change)

	// deliver update notifications
	switch s.mode {
	case PodConfigNotificationIncremental:
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}
		if len(adds.Pods) > 0 {
			s.updates <- *adds
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}

	case PodConfigNotificationSnapshotAndUpdates:
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 || len(adds.Pods) > 0 {
			s.updates <- kubelet.PodUpdate{s.MergedState().([]kubelet.Pod), kubelet.SET}
		}

	case PodConfigNotificationSnapshot:
		if len(updates.Pods) > 0 || len(deletes.Pods) > 0 || len(adds.Pods) > 0 {
			s.updates <- kubelet.PodUpdate{s.MergedState().([]kubelet.Pod), kubelet.SET}
		}

	default:
		panic(fmt.Sprintf("unsupported PodConfigNotificationMode: %#v", s.mode))
	}

	return nil
}
// merge pods
func (s *podStorage) merge(source string, change interface{}) (adds, updates, deletes *kubelet.PodUpdate) {
	s.podLock.Lock()
	defer s.podLock.Unlock()

	adds = &kubelet.PodUpdate{Op: kubelet.ADD}
	updates = &kubelet.PodUpdate{Op: kubelet.UPDATE}
	deletes = &kubelet.PodUpdate{Op: kubelet.REMOVE}

	pods := s.pods[source]
	if pods == nil {
		pods = make(map[string]*kubelet.Pod)
	}

	update := change.(kubelet.PodUpdate)
	switch update.Op {
	case kubelet.ADD, kubelet.UPDATE: // 要更新的直接更新然后放到updates，新加的先放到pods直接放到adds
		if update.Op == kubelet.ADD {
			glog.Infof("Adding new pods from source %s : %v", source, update.Pods)
		} else {
			glog.Infof("Updating pods from source %s : %v", source, update.Pods)
		}

		filtered := filterInvalidPods(update.Pods, source)
		for _, ref := range filtered {
			name := ref.Name
			if existing, found := pods[name]; found {
				if !reflect.DeepEqual(existing.Manifest, ref.Manifest) {
					// this is an update
					existing.Manifest = ref.Manifest
					updates.Pods = append(updates.Pods, *existing)
					continue
				}
				// this is a no-op
				continue
			}
			// this is an add
			ref.Namespace = source
			pods[name] = ref
			adds.Pods = append(adds.Pods, *ref)
		}

	case kubelet.REMOVE:
		glog.Infof("Removing a pod %v", update)
		for _, value := range update.Pods {
			name := value.Name
			if existing, found := pods[name]; found {
				// this is a delete
				delete(pods, name)
				deletes.Pods = append(deletes.Pods, *existing)
				continue
			}
			// this is a no-op
		}

	case kubelet.SET: // 看起来即使是set操作也是会根据在老得pods中是否存在分为update，add，delete
		glog.Infof("Setting pods for source %s : %v", source, update)
		// Clear the old map entries by just creating a new map
		oldPods := pods
		pods = make(map[string]*kubelet.Pod)

		filtered := filterInvalidPods(update.Pods, source)
		for _, ref := range filtered {
			name := ref.Name
			if existing, found := oldPods[name]; found {
				pods[name] = existing
				if !reflect.DeepEqual(existing.Manifest, ref.Manifest) {
					// this is an update
					existing.Manifest = ref.Manifest
					updates.Pods = append(updates.Pods, *existing)
					continue
				}
				// this is a no-op
				continue
			}
			ref.Namespace = source
			pods[name] = ref
			adds.Pods = append(adds.Pods, *ref)
		}

		for name, existing := range oldPods {
			if _, found := pods[name]; !found {
				// this is a delete
				deletes.Pods = append(deletes.Pods, *existing)
			}
		}

	default:
		glog.Infof("Received invalid update type: %v", update)

	}

	s.pods[source] = pods
	return adds, updates, deletes
}
// 过滤掉有问题的pod
func filterInvalidPods(pods []kubelet.Pod, source string) (filtered []*kubelet.Pod) {
	names := util.StringSet{}
	for i := range pods {
		var errors []error
		if names.Has(pods[i].Name) {
			errors = append(errors, apierrs.NewFieldDuplicate("name", pods[i].Name))
		} else {
			names.Insert(pods[i].Name)
		}
		if errs := kubelet.ValidatePod(&pods[i]); len(errs) != 0 {
			errors = append(errors, errs...)
		}
		if len(errors) > 0 {
			glog.Warningf("Pod %d from %s failed validation, ignoring: %v", i+1, source, errors)
			continue
		}
		filtered = append(filtered, &pods[i])
	}
	return
}
// 就是把快照发到updates
// Sync sends a copy of the current state through the update channel.
func (s *podStorage) Sync() {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()
	s.updates <- kubelet.PodUpdate{s.MergedState().([]kubelet.Pod), kubelet.SET}
}

// Object implements config.Accessor
func (s *podStorage) MergedState() interface{} {
	s.podLock.RLock()
	defer s.podLock.RUnlock()
	pods := make([]kubelet.Pod, 0)
	for source, sourcePods := range s.pods {
		for _, podRef := range sourcePods {
			pod := *podRef
			pod.Namespace = source
			pods = append(pods, pod)
		}
	}
	return pods
}
