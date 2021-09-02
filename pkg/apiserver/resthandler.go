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
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/httplog"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
)

/*
这个函数其实就是对外提供接口，让外部可以对用api对object进行增删改查
*/

type RESTHandler struct {
	storage     map[string]RESTStorage
	codec       runtime.Codec
	ops         *Operations
	asyncOpWait time.Duration   // 对某个操作要等多长时间
}

// ServeHTTP handles requests to all RESTStorage objects.
func (h *RESTHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	parts := splitPath(req.URL.Path)
	if len(parts) < 1 {
		notFound(w, req)
		return
	}
	storage := h.storage[parts[0]]
	if storage == nil {
		httplog.LogOf(w).Addf("'%v' has no storage object", parts[0])
		notFound(w, req)
		return
	}

	h.handleRESTStorage(parts, req, w, storage)
}

// handleRESTStorage is the main dispatcher for a storage object.  It switches on the HTTP method, and then
// on path length, according to the following table:
//   Method     Path          Action
//   GET        /foo          list
//   GET        /foo/bar      get 'bar'
//   POST       /foo          create
//   PUT        /foo/bar      update 'bar'
//   DELETE     /foo/bar      delete 'bar'
// Returns 404 if the method/pattern doesn't match one of these entries
// The s accepts several query parameters:
//    sync=[false|true] Synchronous request (only applies to create, update, delete operations)
//    timeout=<duration> Timeout for synchronous requests, only applies if sync=true
//    labels=<label-selector> Used for filtering list operations
func (h *RESTHandler) handleRESTStorage(parts []string, req *http.Request, w http.ResponseWriter, storage RESTStorage) {
	sync := req.URL.Query().Get("sync") == "true"            //获取sync参数，判断是否都是同步请求
	timeout := parseTimeout(req.URL.Query().Get("timeout"))  // 获取超时时间
	switch req.Method {
	case "GET":
		switch len(parts) {
		case 1:   // 调用list函数，根据labels获取对应的资源 LIST操作。并返回信息
			selector, err := labels.ParseSelector(req.URL.Query().Get("labels"))
			if err != nil {
				errorJSON(err, h.codec, w)
				return
			}
			list, err := storage.List(selector)
			if err != nil {
				errorJSON(err, h.codec, w)
				return
			}
			writeJSON(http.StatusOK, h.codec, list, w)
		case 2:  // 调用GET函数，根据labels获取对应的资源 GET操作
			item, err := storage.Get(parts[1])
			if err != nil {
				errorJSON(err, h.codec, w)
				return
			}
			writeJSON(http.StatusOK, h.codec, item, w)
		default:
			notFound(w, req)
		}

	case "POST":  // 创建一个object，并记录operation状态，，最后把该状态返回给用户
		if len(parts) != 1 {
			notFound(w, req)
			return
		}
		body, err := readBody(req)
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		obj := storage.New()
		err = h.codec.DecodeInto(body, obj)
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		out, err := storage.Create(obj)
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		op := h.createOperation(out, sync, timeout)
		h.finishReq(op, w)

	case "DELETE":  // 删除一个object，并记录operation，最后返回给用户operation状态
		if len(parts) != 2 {
			notFound(w, req)
			return
		}
		out, err := storage.Delete(parts[1])
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		op := h.createOperation(out, sync, timeout)
		h.finishReq(op, w)

	case "PUT":  //  更新一个object，并记录operation，最后返回给用户operation状态
		if len(parts) != 2 {
			notFound(w, req)
			return
		}
		body, err := readBody(req)
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		obj := storage.New()
		err = h.codec.DecodeInto(body, obj)
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		out, err := storage.Update(obj)
		if err != nil {
			errorJSON(err, h.codec, w)
			return
		}
		op := h.createOperation(out, sync, timeout)
		h.finishReq(op, w)

	default:
		notFound(w, req)
	}
}
// 创建一个operation,并等待operation完成(异步的话直接返回operation)。（就是等待out出结果）
// createOperation creates an operation to process a channel response.
func (h *RESTHandler) createOperation(out <-chan runtime.Object, sync bool, timeout time.Duration) *Operation {
	op := h.ops.NewOperation(out)
	if sync {
		op.WaitFor(timeout)
	} else if h.asyncOpWait != 0 {
		op.WaitFor(h.asyncOpWait)
	}
	return op
}
// 其实就是读取operation的状态并返回
// finishReq finishes up a request, waiting until the operation finishes or, after a timeout, creating an
// Operation to receive the result and returning its ID down the writer.
func (h *RESTHandler) finishReq(op *Operation, w http.ResponseWriter) {
	obj, complete := op.StatusOrResult()
	if complete {
		status := http.StatusOK
		switch stat := obj.(type) {
		case *api.Status:
			if stat.Code != 0 {
				status = stat.Code
			}
		}
		writeJSON(status, h.codec, obj, w)
	} else {
		writeJSON(http.StatusAccepted, h.codec, obj, w)
	}
}
