/*
Copyright 2015 The Kubernetes Authors.

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

package images

import (
	goerrors "errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
)

// StatsProvider is an interface for fetching stats used during image garbage
// collection.
// 获取文件系统使用率
type StatsProvider interface {
	// ImageFsStats returns the stats of the image filesystem.
	ImageFsStats() (*statsapi.FsStats, error)
}

// ImageGCManager is an interface for managing lifecycle of all images.
// Implementation is thread-safe.
// 管理image的生命周期，做垃圾回收
type ImageGCManager interface {
	// Applies the garbage collection policy. Errors include being unable to free
	// enough space as per the garbage collection policy.
	GarbageCollect() error

	// Start async garbage collection of images.
	Start() // 定时更新内存的image状态和缓存

	GetImageList() ([]container.Image, error) // 从缓存获取当前image列表

	// Delete all unused images. 删除所有不用的image
	DeleteUnusedImages() error
}

// ImageGCPolicy is a policy for garbage collecting images. Policy defines an allowed band in
// which garbage collection will be run.
// image垃圾回收参数
type ImageGCPolicy struct {
	// Any usage above this threshold will always trigger garbage collection.
	// This is the highest usage we will allow.
	HighThresholdPercent int // 使用空间超过这个值将触发GC

	// Any usage below this threshold will never trigger garbage collection.
	// This is the lowest threshold we will try to garbage collect to.
	LowThresholdPercent int // 小于这个值不会进行GC，进行GC时要尽量让gc之后的剩余空间小于这个值

	// Minimum age at which an image can be garbage collected.
	MinAge time.Duration  // image闲置时间要超过这个值才能被删除
}

// ImageGCManager实现
type realImageGCManager struct {
	// Container runtime
	runtime container.Runtime

	// Records of images and their use.
	// 记录当前节点上的镜像
	imageRecords     map[string]*imageRecord
	imageRecordsLock sync.Mutex

	// The image garbage collection policy in use.
	policy ImageGCPolicy

	// statsProvider provides stats used during image garbage collection.
	statsProvider StatsProvider

	// Recorder for Kubernetes events.
	recorder record.EventRecorder

	// Reference to this node.
	nodeRef *v1.ObjectReference

	// Track initialization
	initialized bool

	// imageCache is the cache of latest image list.
	imageCache imageCache  // 当前机器的image的缓存

	// sandbox image exempted from GC
	sandboxImage string
}

// imageCache caches latest result of ListImages.
type imageCache struct {
	// sync.Mutex is the mutex protects the image cache.
	sync.Mutex
	// images is the image cache.
	images []container.Image
}

// set sorts the input list and updates image cache.
// 'i' takes ownership of the list, you should not reference the list again
// after calling this function.
func (i *imageCache) set(images []container.Image) {
	i.Lock()
	defer i.Unlock()
	// The image list needs to be sorted when it gets read and used in
	// setNodeStatusImages. We sort the list on write instead of on read,
	// because the image cache is more often read than written
	sort.Sort(sliceutils.ByImageSize(images))
	i.images = images
}

// get gets image list from image cache.
// NOTE: The caller of get() should not do mutating operations on the
// returned list that could cause data race against other readers (e.g.
// in-place sorting the returned list)
func (i *imageCache) get() []container.Image {
	i.Lock()
	defer i.Unlock()
	return i.images
}

// Information about the images we track.
// 缓存的image使用信息，所有在本机的image都会有这个记录
type imageRecord struct {
	// Time when this image was first detected.
	firstDetected time.Time // 第一次detect到它的时间

	// Time when we last saw this image being used.
	lastUsed time.Time  // 上一次监测到它被pod使用事件

	// Size of the image in bytes.
	size int64 // 镜像大小
}

// NewImageGCManager instantiates a new ImageGCManager object.
func NewImageGCManager(runtime container.Runtime, statsProvider StatsProvider, recorder record.EventRecorder, nodeRef *v1.ObjectReference, policy ImageGCPolicy, sandboxImage string) (ImageGCManager, error) {
	// Validate policy.
	if policy.HighThresholdPercent < 0 || policy.HighThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid HighThresholdPercent %d, must be in range [0-100]", policy.HighThresholdPercent)
	}
	if policy.LowThresholdPercent < 0 || policy.LowThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid LowThresholdPercent %d, must be in range [0-100]", policy.LowThresholdPercent)
	}
	if policy.LowThresholdPercent > policy.HighThresholdPercent {
		return nil, fmt.Errorf("LowThresholdPercent %d can not be higher than HighThresholdPercent %d", policy.LowThresholdPercent, policy.HighThresholdPercent)
	}
	im := &realImageGCManager{
		runtime:       runtime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: statsProvider,
		recorder:      recorder,
		nodeRef:       nodeRef,
		initialized:   false,
		sandboxImage:  sandboxImage,
	}

	return im, nil
}
//Start 1.定时检测正在使用的image 2.定时把当前机器所有的节点放到cache缓存中
func (im *realImageGCManager) Start() {
	go wait.Until(func() {
		// Initial detection make detected time "unknown" in the past.
		var ts time.Time
		if im.initialized {
			ts = time.Now()
		}
		// 定时检测正在使用的image
		_, err := im.detectImages(ts)
		if err != nil {
			klog.InfoS("Failed to monitor images", "err", err)
		} else {
			im.initialized = true
		}
	}, 5*time.Minute, wait.NeverStop)

	// Start a goroutine periodically updates image cache.
	// 定时把当前机器所有的节点放到cache缓存中
	go wait.Until(func() {
		images, err := im.runtime.ListImages()
		if err != nil {
			klog.InfoS("Failed to update image list", "err", err)
		} else {
			im.imageCache.set(images)
		}
	}, 30*time.Second, wait.NeverStop)

}

// Get a list of images on this node
// 当前节点的所有image，这个是从缓存取
func (im *realImageGCManager) GetImageList() ([]container.Image, error) {
	return im.imageCache.get(), nil
}

// 返回的值的 sandboxImage+pod使用的image的集合（正在被使用的image集合），更新imageRecords
func (im *realImageGCManager) detectImages(detectTime time.Time) (sets.String, error) {
	imagesInUse := sets.NewString()

	// Always consider the container runtime pod sandbox image in use
	// sandboxImage使用的image
	imageRef, err := im.runtime.GetImageRef(container.ImageSpec{Image: im.sandboxImage})
	if err == nil && imageRef != "" {
		imagesInUse.Insert(imageRef)
	}
    // 当前机器上的所有的image
	images, err := im.runtime.ListImages()
	if err != nil {
		return imagesInUse, err
	}
	// 所有pods
	pods, err := im.runtime.GetPods(true)
	if err != nil {
		return imagesInUse, err
	}

	// Make a set of images in use by containers.
	// 所有被pods使用的镜像
	for _, pod := range pods {
		for _, container := range pod.Containers {
			klog.V(5).InfoS("Container uses image", "pod", klog.KRef(pod.Namespace, pod.Name), "containerName", container.Name, "containerImage", container.Image, "imageID", container.ImageID)
			imagesInUse.Insert(container.ImageID)
		}
	}

	// Add new images and record those being used.
	now := time.Now()
	currentImages := sets.NewString() // 当前节点上所有镜像
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	for _, image := range images {
		klog.V(5).InfoS("Adding image ID to currentImages", "imageID", image.ID)
		currentImages.Insert(image.ID)

		// New image, set it as detected now.
		if _, ok := im.imageRecords[image.ID]; !ok {
			klog.V(5).InfoS("Image ID is new", "imageID", image.ID)
			im.imageRecords[image.ID] = &imageRecord{
				firstDetected: detectTime,
			}
		}

		// Set last used time to now if the image is being used.
		// 判断该image是否有pod正在使用，如果有就设置最后一次使用的事件
		if isImageUsed(image.ID, imagesInUse) {
			klog.V(5).InfoS("Setting Image ID lastUsed", "imageID", image.ID, "lastUsed", now)
			im.imageRecords[image.ID].lastUsed = now
		}

		klog.V(5).InfoS("Image ID has size", "imageID", image.ID, "size", image.Size)
		//
		im.imageRecords[image.ID].size = image.Size // 更新镜像大小
	}

	// Remove old images from our records.
	// 移除不用的的image
	for image := range im.imageRecords {
		if !currentImages.Has(image) {
			klog.V(5).InfoS("Image ID is no longer present; removing from imageRecords", "imageID", image)
			delete(im.imageRecords, image)
		}
	}

	return imagesInUse, nil
}

// GarbageCollect 根据文件系统信息，和收集的image使用情况，还有配置的GC阀值来进行image回收
func (im *realImageGCManager) GarbageCollect() error {
	// Get disk usage on disk holding images.
	// 获取文件系统使用信息
	fsStats, err := im.statsProvider.ImageFsStats()
	if err != nil {
		return err
	}

	var capacity, available int64
	if fsStats.CapacityBytes != nil {
		capacity = int64(*fsStats.CapacityBytes)
	}
	if fsStats.AvailableBytes != nil {
		available = int64(*fsStats.AvailableBytes)
	}

	if available > capacity {
		klog.InfoS("Availability is larger than capacity", "available", available, "capacity", capacity)
		available = capacity
	}

	// Check valid capacity.
	if capacity == 0 {
		err := goerrors.New("invalid capacity 0 on image filesystem")
		im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.InvalidDiskCapacity, err.Error())
		return err
	}

	// If over the max threshold, free enough to place us at the lower threshold.
	// 已使用的空间比例
	usagePercent := 100 - int(available*100/capacity)
	if usagePercent >= im.policy.HighThresholdPercent { // 使用空间超过阀值上限，触发GC
		// 要释放amountToFree空间才能到达LowThresholdPercent的情况
		amountToFree := capacity*int64(100-im.policy.LowThresholdPercent)/100 - available
		klog.InfoS("Disk usage on image filesystem is over the high threshold, trying to free bytes down to the low threshold", "usage", usagePercent, "highThreshold", im.policy.HighThresholdPercent, "amountToFree", amountToFree, "lowThreshold", im.policy.LowThresholdPercent)
		freed, err := im.freeSpace(amountToFree, time.Now())  // image回收
		if err != nil {
			return err
		}
        // 释放的空间没有达到要求
		if freed < amountToFree {
			err := fmt.Errorf("failed to garbage collect required amount of images. Wanted to free %d bytes, but freed %d bytes", amountToFree, freed)
			im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.FreeDiskSpaceFailed, err.Error())
			return err
		}
	}

	return nil
}

// DeleteUnusedImages 尽量把所有符合要求的闲置image都删除
func (im *realImageGCManager) DeleteUnusedImages() error {
	klog.InfoS("Attempting to delete unused images")
	_, err := im.freeSpace(math.MaxInt64, time.Now())
	return err
}

// Tries to free bytesToFree worth of images on the disk.
//
// Returns the number of bytes free and an error if any occurred. The number of
// bytes freed is always returned.
// Note that error may be nil and the number of bytes free may be less
// than bytesToFree.
// 累计删除指定大小bytesToFree的image
// 使用detectImages得到所有正在被使用的image，然后根据imageRecords找到没有被使用的image，然后挨个删除，直到最后删除的空间满足了imageRecords大小
func (im *realImageGCManager) freeSpace(bytesToFree int64, freeTime time.Time) (int64, error) {
	imagesInUse, err := im.detectImages(freeTime)
	if err != nil {
		return 0, err
	}

	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()

	// Get all images in eviction order.
	images := make([]evictionInfo, 0, len(im.imageRecords))
	// 获取所有没有在使用image
	for image, record := range im.imageRecords {
		if isImageUsed(image, imagesInUse) {
			klog.V(5).InfoS("Image ID is being used", "imageID", image)
			continue
		}
		images = append(images, evictionInfo{
			id:          image,
			imageRecord: *record,
		})
	}
	sort.Sort(byLastUsedAndDetected(images))

	// Delete unused images until we've freed up enough space.
	var deletionErrors []error
	spaceFreed := int64(0)  // 记录已经删除的image的大小
	for _, image := range images {
		klog.V(5).InfoS("Evaluating image ID for possible garbage collection", "imageID", image.id)
		// Images that are currently in used were given a newer lastUsed.
		if image.lastUsed.Equal(freeTime) || image.lastUsed.After(freeTime) {
			klog.V(5).InfoS("Image ID was used too recently, not eligible for garbage collection", "imageID", image.id, "lastUsed", image.lastUsed, "freeTime", freeTime)
			continue
		}

		// Avoid garbage collect the image if the image is not old enough.
		// In such a case, the image may have just been pulled down, and will be used by a container right away.

		if freeTime.Sub(image.firstDetected) < im.policy.MinAge {
			klog.V(5).InfoS("Image ID's age is less than the policy's minAge, not eligible for garbage collection", "imageID", image.id, "age", freeTime.Sub(image.firstDetected), "minAge", im.policy.MinAge)
			continue
		}

		// Remove image. Continue despite errors.
		klog.InfoS("Removing image to free bytes", "imageID", image.id, "size", image.size)
		// 删除image
		err := im.runtime.RemoveImage(container.ImageSpec{Image: image.id})
		if err != nil {
			deletionErrors = append(deletionErrors, err)
			continue
		}
		delete(im.imageRecords, image.id)
		spaceFreed += image.size

		if spaceFreed >= bytesToFree { // 回收够了直接返回
			break
		}
	}

	if len(deletionErrors) > 0 {
		return spaceFreed, fmt.Errorf("wanted to free %d bytes, but freed %d bytes space with errors in image deletion: %v", bytesToFree, spaceFreed, errors.NewAggregate(deletionErrors))
	}
	return spaceFreed, nil
}

type evictionInfo struct {
	id string
	imageRecord
}

type byLastUsedAndDetected []evictionInfo
// firstDetected/lastUsed比较早的先被回收，因为这种比较可能以后不再被使用
func (ev byLastUsedAndDetected) Len() int      { return len(ev) }
func (ev byLastUsedAndDetected) Swap(i, j int) { ev[i], ev[j] = ev[j], ev[i] }
func (ev byLastUsedAndDetected) Less(i, j int) bool {
	// Sort by last used, break ties by detected.
	if ev[i].lastUsed.Equal(ev[j].lastUsed) {
		return ev[i].firstDetected.Before(ev[j].firstDetected)
	}
	return ev[i].lastUsed.Before(ev[j].lastUsed)
}
// 判断imageID是否(有pod)正在被使用
func isImageUsed(imageID string, imagesInUse sets.String) bool {
	// Check the image ID.
	if _, ok := imagesInUse[imageID]; ok {
		return true
	}
	return false
}
