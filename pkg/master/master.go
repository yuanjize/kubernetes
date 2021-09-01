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
	"net/http"
	"time"

	_ "github.com/GoogleCloudPlatform/kubernetes/pkg/api/v1beta1"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/apiserver"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/binding"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/controller"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/endpoint"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/etcd"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/minion"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/pod"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/service"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	servicecontroller "github.com/GoogleCloudPlatform/kubernetes/pkg/service"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"

	goetcd "github.com/coreos/go-etcd/etcd"
	"github.com/golang/glog"
)

// Config is a structure used to configure a Master.
type Config struct {
	Client             *client.Client
	Cloud              cloudprovider.Interface
	EtcdServers        []string
	HealthCheckMinions bool
	Minions            []string   //所有的node
	MinionCacheTTL     time.Duration
	MinionRegexp       string
	PodInfoGetter      client.PodInfoGetter  // http api,用来访问具体的pod节点
}
/*
Registry都是Etcd接口，用来访问ETCD的
storage是apiserver对外提供的服务
client 是用来访问节点的API
*/
// Master contains state for a Kubernetes cluster master/api server.
type Master struct {
	podRegistry        pod.Registry
	controllerRegistry controller.Registry
	serviceRegistry    service.Registry
	endpointRegistry   endpoint.Registry
	minionRegistry     minion.Registry
	bindingRegistry    binding.Registry
	storage            map[string]apiserver.RESTStorage // 一些本服务监听的http接口
	client             *client.Client
}

// New returns a new instance of Master connected to the given etcdServer. 初始了一些访问etcd的api
func New(c *Config) *Master {
	etcdClient := goetcd.NewClient(c.EtcdServers)
	minionRegistry := makeMinionRegistry(c)  //初始化节点仓库
	//这里其实就是初始化一些etcdAPI,podRegistry其实就是一些对特定数据api进行封装
	m := &Master{
		podRegistry:        etcd.NewRegistry(etcdClient),
		controllerRegistry: etcd.NewRegistry(etcdClient),
		serviceRegistry:    etcd.NewRegistry(etcdClient),
		endpointRegistry:   etcd.NewRegistry(etcdClient),
		bindingRegistry:    etcd.NewRegistry(etcdClient),
		minionRegistry:     minionRegistry,  // 访问节点用,这些信息基本都在内存里，没有用etcd
		client:             c.Client,        // 一些http api，看起来是可以直接访问资源实体，例如直接访问Pod节点
	}
	m.init(c.Cloud, c.PodInfoGetter)
	return m
}
// 用来获取node list的，只看第一个实现，起的实现不用管
func makeMinionRegistry(c *Config) minion.Registry {
	var minionRegistry minion.Registry
	if c.Cloud != nil && len(c.MinionRegexp) > 0 {
		var err error
		minionRegistry, err = minion.NewCloudRegistry(c.Cloud, c.MinionRegexp)
		if err != nil {
			glog.Errorf("Failed to initalize cloud minion registry reverting to static registry (%#v)", err)
		}
	}
	if minionRegistry == nil {
		minionRegistry = minion.NewRegistry(c.Minions)
	}
	if c.HealthCheckMinions {
		minionRegistry = minion.NewHealthyRegistry(minionRegistry, &http.Client{})  // 可以认为是一个wrapper，获取机器信息的时候会进行健康检查过滤掉不健康的
	}
	if c.MinionCacheTTL > 0 {
		cachingMinionRegistry, err := minion.NewCachingRegistry(minionRegistry, c.MinionCacheTTL)
		if err != nil {
			glog.Errorf("Failed to initialize caching layer, ignoring cache.")
		} else {
			minionRegistry = cachingMinionRegistry
		}
	}
	return minionRegistry
}

func (m *Master) init(cloud cloudprovider.Interface, podInfoGetter client.PodInfoGetter) {
	podCache := NewPodCache(podInfoGetter, m.podRegistry)
	go util.Forever(func() { podCache.UpdateAllContainers() }, time.Second*30)  // 定时更新所有的pod信息到pod cache

	endpoints := servicecontroller.NewEndpointController(m.serviceRegistry, m.client)
	go util.Forever(func() { endpoints.SyncServiceEndpoints() }, time.Second*10)  //定时更新服务对应的 endpoints到etcd

	m.storage = map[string]apiserver.RESTStorage{
		"pods": pod.NewREST(&pod.RESTConfig{
			CloudProvider: cloud,
			PodCache:      podCache,
			PodInfoGetter: podInfoGetter,
			Registry:      m.podRegistry,
		}),
		"replicationControllers": controller.NewREST(m.controllerRegistry, m.podRegistry),
		"services":               service.NewREST(m.serviceRegistry, cloud, m.minionRegistry),
		"endpoints":              endpoint.NewREST(m.endpointRegistry),
		"minions":                minion.NewREST(m.minionRegistry),

		// TODO: should appear only in scheduler API group.
		"bindings": binding.NewREST(m.bindingRegistry),
	}
}

// API_v1beta1 returns the resources and codec for API version v1beta1.
func (m *Master) API_v1beta1() (map[string]apiserver.RESTStorage, runtime.Codec) {
	storage := make(map[string]apiserver.RESTStorage)
	for k, v := range m.storage {
		storage[k] = v
	}
	return storage, runtime.DefaultCodec
}
