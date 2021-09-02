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

package apiserver

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
)
/*
这些代码是用来记录执行的操作的集合

下面的handle是用来获取operation，看起来只返回opid
*/
type OperationHandler struct {
	ops   *Operations
	codec runtime.Codec
}

func (h *OperationHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	parts := splitPath(req.URL.Path)
	if len(parts) > 1 || req.Method != "GET" {
		notFound(w, req)
		return
	}
	if len(parts) == 0 {
		// List outstanding operations.
		list := h.ops.List()
		writeJSON(http.StatusOK, h.codec, list, w)
		return
	}

	op := h.ops.Get(parts[0])
	if op == nil {
		notFound(w, req)
		return
	}

	obj, complete := op.StatusOrResult()
	if complete {
		writeJSON(http.StatusOK, h.codec, obj, w)
	} else {
		writeJSON(http.StatusAccepted, h.codec, obj, w)
	}
}
// Operation 是一个正在进行的操作
// Operation represents an ongoing action which the server is performing.
type Operation struct {
	ID       string
	result   runtime.Object         //操作结果
	awaiting <-chan runtime.Object  // 用来等待操作的结果
	finished *time.Time             //操作完成时间
	lock     sync.Mutex
	notify   chan struct{}          // 用来通知操作完成
}

// Operations tracks all the ongoing operations.
type Operations struct {
	// Access only using functions from atomic.
	lastID int64  // 操作id，是一个自增值

	// 'lock' guards the ops map.
	lock sync.Mutex
	ops  map[string]*Operation //当前未完成操作的集合，定时会清理已经完成的操作。
}

// NewOperations returns a new Operations repository.
func NewOperations() *Operations {
	ops := &Operations{
		ops: map[string]*Operation{},
	}
	go util.Forever(func() { ops.expire(10 * time.Minute) }, 5*time.Minute)
	return ops
}
// 创建一个Operation，并加入当前的操作集合，并在单独的线程中国调用wait等待操作完成。 当from读成功的时候就代表任务已经完成。其实就是等待from，from就是代表任务
// NewOperation adds a new operation. It is lock-free.
func (ops *Operations) NewOperation(from <-chan runtime.Object) *Operation {
	id := atomic.AddInt64(&ops.lastID, 1)
	op := &Operation{
		ID:       strconv.FormatInt(id, 10),
		awaiting: from,
		notify:   make(chan struct{}),
	}
	go op.wait()
	go ops.insert(op)
	return op
}

// insert inserts op into the ops map.
func (ops *Operations) insert(op *Operation) {
	ops.lock.Lock()
	defer ops.lock.Unlock()
	ops.ops[op.ID] = op
}

// List lists operations for an API client.
func (ops *Operations) List() *api.ServerOpList {
	ops.lock.Lock()
	defer ops.lock.Unlock()

	ids := []string{}
	for id := range ops.ops {
		ids = append(ids, id)
	}
	sort.StringSlice(ids).Sort()
	ol := &api.ServerOpList{}
	for _, id := range ids {
		ol.Items = append(ol.Items, api.ServerOp{JSONBase: api.JSONBase{ID: id}})
	}
	return ol
}

// Get returns the operation with the given ID, or nil.
func (ops *Operations) Get(id string) *Operation {
	ops.lock.Lock()
	defer ops.lock.Unlock()
	return ops.ops[id]
}

// expire garbage collect operations that have finished longer than maxAge ago.
// maxAge秒钟之前就已经完成的操作从集合中移除
func (ops *Operations) expire(maxAge time.Duration) {
	ops.lock.Lock()
	defer ops.lock.Unlock()
	keep := map[string]*Operation{}
	limitTime := time.Now().Add(-maxAge)
	for id, op := range ops.ops {
		if !op.expired(limitTime) {
			keep[id] = op
		}
	}
	ops.ops = keep
}

// wait waits forever for the operation to complete; call via go when
// the operation is created. Sets op.finished when the operation
// does complete, and closes the notify channel, in case there
// are any WaitFor() calls in progress.
// Does not keep op locked while waiting.
// 一直等待，直到操作完成
func (op *Operation) wait() {
	defer util.HandleCrash()
	result := <-op.awaiting

	op.lock.Lock()
	defer op.lock.Unlock()
	op.result = result
	finished := time.Now()
	op.finished = &finished
	close(op.notify)
}

// WaitFor waits for the specified duration, or until the operation finishes,
// whichever happens first.
// 上面操作的超时本版本
func (op *Operation) WaitFor(timeout time.Duration) {
	select {
	case <-time.After(timeout):
	case <-op.notify:
	}
}

// expired returns true if this operation finished before limitTime.
// 操作在limitTime时间点之前已经完成了，那么是操作过期了
func (op *Operation) expired(limitTime time.Time) bool {
	op.lock.Lock()
	defer op.lock.Unlock()
	if op.finished == nil {
		return false
	}
	return op.finished.Before(limitTime)
}
//返回当前操作的状态，是是已经完成还是正在进行
// StatusOrResult returns status information or the result of the operation if it is complete,
// with a bool indicating true in the latter case.
func (op *Operation) StatusOrResult() (description runtime.Object, finished bool) {
	op.lock.Lock()
	defer op.lock.Unlock()

	if op.finished == nil {
		return &api.Status{
			Status:  api.StatusWorking,
			Reason:  api.StatusReasonWorking,
			Details: &api.StatusDetails{ID: op.ID, Kind: "operation"},
		}, false
	}
	return op.result, true
}
