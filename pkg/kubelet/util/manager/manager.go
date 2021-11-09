/*
Copyright 2018 The Kubernetes Authors.

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

package manager

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Manager is the interface for registering and unregistering
// objects referenced by pods in the underlying cache and
// extracting those from that cache if needed.
/*
 用来registering和unregistering被Pod使用的资源
   1.GetObject 获取Object
   2.RegisterPod 注册Pod引用的所有资源
   3.UnregisterPod Pod不再使用引用的所有资源。这两个函数一般都用引用计数法来标志资源的引用
*/
type Manager interface {
	// Get object by its namespace and name.
	GetObject(namespace, name string) (runtime.Object, error)

	// WARNING: Register/UnregisterPod functions should be efficient,
	// i.e. should not block on network operations.

	// RegisterPod registers all objects referenced from a given pod.
	//
	// NOTE: All implementations of RegisterPod should be idempotent.
	RegisterPod(pod *v1.Pod)

	// UnregisterPod unregisters objects referenced from a given pod that are not
	// used by any other registered pod.
	//
	// NOTE: All implementations of UnregisterPod should be idempotent.
	UnregisterPod(pod *v1.Pod)
}

// Store is the interface for a object cache that
// can be used by cacheBasedManager.
/*
  Object的内存缓存
   1.AddReference添加对资源的引用
   2.DeleteReference减少对资源的引用
   3.GetObject获取object
*/
type Store interface {
	// AddReference adds a reference to the object to the store.
	// Note that multiple additions to the store has to be allowed
	// in the implementations and effectively treated as refcounted.
	AddReference(namespace, name string)
	// DeleteReference deletes reference to the object from the store.
	// Note that object should be deleted only when there was a
	// corresponding Delete call for each of Add calls (effectively
	// when refcount was reduced to zero).
	DeleteReference(namespace, name string)
	// Get an object from a store.
	Get(namespace, name string) (runtime.Object, error)
}
