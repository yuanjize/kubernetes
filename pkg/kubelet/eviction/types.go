/*
Copyright 2016 The Kubernetes Authors.

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

package eviction

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
)

// fsStatsType defines the types of filesystem stats to collect.
type fsStatsType string

const (
	// fsStatsLocalVolumeSource identifies stats for pod local volume sources.
	fsStatsLocalVolumeSource fsStatsType = "localVolumeSource"
	// fsStatsLogs identifies stats for pod logs.
	fsStatsLogs fsStatsType = "logs"
	// fsStatsRoot identifies stats for pod container writable layers.
	fsStatsRoot fsStatsType = "root"
)

// Config holds information about how eviction is configured.
// eviction配置信息
type Config struct {
	// PressureTransitionPeriod is duration the kubelet has to wait before transitioning out of a pressure condition.
	// kubelet 在退出压力条件之前必须等待的持续时间
	PressureTransitionPeriod time.Duration
	// Maximum allowed grace period (in seconds) to use when terminating pods in response to a soft eviction threshold being met.
	// 对pod软驱逐的时候最大等待时间
	MaxPodGracePeriodSeconds int64
	// Thresholds define the set of conditions monitored to trigger eviction.
	// 定义触发驱逐的条件集合
	Thresholds []evictionapi.Threshold
	// KernelMemcgNotification if true will integrate with the kernel memcg notification to determine if memory thresholds are crossed.
	// 和内核的memcg（memory cgroup）集成，来接受内存超过阀值的通知
	KernelMemcgNotification bool
	// PodCgroupRoot is the cgroup which contains all pods.
	// 包含所有的pod的cgroup
	PodCgroupRoot string
}

// Manager evaluates when an eviction threshold for node stability has been met on the node.
// 评估是否有资源到达了eviction阀值
type Manager interface {
	// Start starts the control loop to monitor eviction thresholds at specified interval.
	// 开启喜欢，监控eviction阀值
	Start(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc, podCleanedUpFunc PodCleanedUpFunc, monitoringInterval time.Duration)

	// IsUnderMemoryPressure returns true if the node is under memory pressure.
	IsUnderMemoryPressure() bool

	// IsUnderDiskPressure returns true if the node is under disk pressure.
	IsUnderDiskPressure() bool

	// IsUnderPIDPressure returns true if the node is under PID pressure.
	IsUnderPIDPressure() bool
}

// DiskInfoProvider is responsible for informing the manager how disk is configured.
// 负责通知manager磁盘的配置
type DiskInfoProvider interface {
	// HasDedicatedImageFs returns true if the imagefs is on a separate device from the rootfs.
	HasDedicatedImageFs() (bool, error)
}

// ImageGC is responsible for performing garbage collection of unused images.
// 负责对不用的Image进行GC
type ImageGC interface {
	// DeleteUnusedImages deletes unused images.
	DeleteUnusedImages() error
}

// ContainerGC is responsible for performing garbage collection of unused containers.
// 负责对不用的容器进行GC
type ContainerGC interface {
	// DeleteAllUnusedContainers deletes all unused containers, even those that belong to pods that are terminated, but not deleted.
	DeleteAllUnusedContainers() error
}

// KillPodFunc kills a pod.
// The pod status is updated, and then it is killed with the specified grace period.
// This function must block until either the pod is killed or an error is encountered.
// Arguments:
// pod - the pod to kill
// status - the desired status to associate with the pod (i.e. why its killed)
// gracePeriodOverride - the grace period override to use instead of what is on the pod spec
// 用来杀死Pod
type KillPodFunc func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, fn func(*v1.PodStatus)) error

// MirrorPodFunc returns the mirror pod for the given static pod and
// whether it was known to the pod manager.
// 返回static pod对应的 mirror pod，bool代表manager是否缓存了该Pod
type MirrorPodFunc func(*v1.Pod) (*v1.Pod, bool)

// ActivePodsFunc returns pods bound to the kubelet that are active (i.e. non-terminal state)
// 绑定到当前kubelete的，目前还活跃的pod
type ActivePodsFunc func() []*v1.Pod

// PodCleanedUpFunc returns true if all resources associated with a pod have been reclaimed.
// 关联到该pod的所有资源都已经被回收的话，返回true
type PodCleanedUpFunc func(*v1.Pod) bool

// statsFunc returns the usage stats if known for an input pod.
// 返回pod的使用统计
type statsFunc func(pod *v1.Pod) (statsapi.PodStats, bool)

// rankFunc sorts the pods in eviction order
// 按照驱逐的顺序给Pod数组排序
type rankFunc func(pods []*v1.Pod, stats statsFunc)

// signalObservation is the observed resource usage
type signalObservation struct {
	// The resource capacity
	capacity *resource.Quantity
	// The available resource
	available *resource.Quantity
	// Time at which the observation was taken
	time metav1.Time
}

// signalObservations maps a signal to an observed quantity
// signal映射到metrics
type signalObservations map[evictionapi.Signal]signalObservation

// thresholdsObservedAt maps a threshold to a time that it was observed
type thresholdsObservedAt map[evictionapi.Threshold]time.Time

// nodeConditionsObservedAt maps a node condition to a time that it was observed
type nodeConditionsObservedAt map[v1.NodeConditionType]time.Time

// nodeReclaimFunc is a function that knows how to reclaim a resource from the node without impacting pods.
// 知道如何在不影响pods的情况下回收节点资源
type nodeReclaimFunc func() error

// nodeReclaimFuncs is an ordered list of nodeReclaimFunc
// nodeReclaimFunc集合
type nodeReclaimFuncs []nodeReclaimFunc

// CgroupNotifier generates events from cgroup events
// 生成Cgroup事件写到eventCh里面
type CgroupNotifier interface {
	// Start causes the CgroupNotifier to begin notifying on the eventCh
	Start(eventCh chan<- struct{})
	// Stop stops all processes and cleans up file descriptors associated with the CgroupNotifier
	Stop()
}

// NotifierFactory creates CgroupNotifer
// 根绝cgroup的path创建CgroupNotifier，当attribute超过设置的threshold的时候，CgroupNotifier会同志channel
type NotifierFactory interface {
	// NewCgroupNotifier creates a CgroupNotifier that creates events when the threshold
	// on the attribute in the cgroup specified by the path is crossed.
	NewCgroupNotifier(path, attribute string, threshold int64) (CgroupNotifier, error)
}

// ThresholdNotifier manages CgroupNotifiers based on memory eviction thresholds, and performs a function
// when memory eviction thresholds are crossed
/*
  管理基于memory eviction thresholds的CgroupNotifiers，当到达阀值之后执行一个函数
*/
type ThresholdNotifier interface {
	// Start calls the notifier function when the CgroupNotifier notifies the ThresholdNotifier that an event occurred
	// 开始监听CgroupNotifier的事件
	Start()
	// UpdateThreshold updates the memory cgroup threshold based on the metrics provided.
	// Calling UpdateThreshold with recent metrics allows the ThresholdNotifier to trigger at the
	// eviction threshold more accurately
	// 外部函数会调用该函数，让ThresholdNotifier重新计算Threshold
	UpdateThreshold(summary *statsapi.Summary) error
	// Description produces a relevant string describing the Memory Threshold Notifier
	// 描述Memory Threshold Notifier
	Description() string
}
