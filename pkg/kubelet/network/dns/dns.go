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

package dns

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/util/format"

	"k8s.io/klog/v2"
	utilio "k8s.io/utils/io"
	utilnet "k8s.io/utils/net"
)

var (
	// The default dns opt strings. 5个点
	defaultDNSOptions = []string{"ndots:5"}
)
// 博客讲解：https://medium.com/@hsahu24/understanding-dns-resolution-and-resolv-conf-d17d1d64471c
type podDNSType int

const (
	podDNSCluster podDNSType = iota
	podDNSHost
	podDNSNone
)

const (
	maxResolveConfLength = 10 * 1 << 20 // 10MB
)

// Configurer is used for setting up DNS resolver configuration when launching pods.
// 用来给Pod生成resolve.conf文件
type Configurer struct {
	recorder record.EventRecorder
	nodeRef  *v1.ObjectReference
	nodeIPs  []net.IP

	// If non-nil, use this for container DNS server.
	clusterDNS []net.IP  //集群dns server
	// If non-empty, use this for container DNS search.
	ClusterDomain string // 集群dns search
	// The path to the DNS resolver configuration file used as the base to generate
	// the container's DNS resolver configuration file. This can be used in
	// conjunction with clusterDomain and clusterDNS.
	ResolverConfig string  // resolver配置文件位置
}

// NewConfigurer returns a DNS configurer for launching pods.
func NewConfigurer(recorder record.EventRecorder, nodeRef *v1.ObjectReference, nodeIPs []net.IP, clusterDNS []net.IP, clusterDomain, resolverConfig string) *Configurer {
	return &Configurer{
		recorder:       recorder,
		nodeRef:        nodeRef,
		nodeIPs:        nodeIPs,
		clusterDNS:     clusterDNS,
		ClusterDomain:  clusterDomain,
		ResolverConfig: resolverConfig,
	}
}
// 去除重复字符串
func omitDuplicates(strs []string) []string {
	uniqueStrs := make(map[string]bool)

	var ret []string
	for _, str := range strs {
		if !uniqueStrs[str] {
			ret = append(ret, str)
			uniqueStrs[str] = true
		}
	}
	return ret
}
// 根据一些配置，截断Search字段
func (c *Configurer) formDNSSearchFitsLimits(composedSearch []string, pod *v1.Pod) []string {
	limitsExceeded := false

	maxDNSSearchPaths, maxDNSSearchListChars := validation.MaxDNSSearchPathsLegacy, validation.MaxDNSSearchListCharsLegacy
	if utilfeature.DefaultFeatureGate.Enabled(features.ExpandedDNSConfig) {
		maxDNSSearchPaths, maxDNSSearchListChars = validation.MaxDNSSearchPathsExpanded, validation.MaxDNSSearchListCharsExpanded
	}

	if len(composedSearch) > maxDNSSearchPaths {
		composedSearch = composedSearch[:maxDNSSearchPaths]
		limitsExceeded = true
	}

	// In some DNS resolvers(e.g. glibc 2.28), DNS resolving causes abort() if there is a
	// search path exceeding 255 characters. We have to filter them out.
	l := 0
	for _, search := range composedSearch {
		if len(search) > utilvalidation.DNS1123SubdomainMaxLength {
			limitsExceeded = true
			continue
		}
		composedSearch[l] = search
		l++
	}
	composedSearch = composedSearch[:l]
	// 总长度超过了，那么从后面截断几个
	if resolvSearchLineStrLen := len(strings.Join(composedSearch, " ")); resolvSearchLineStrLen > maxDNSSearchListChars {
		cutDomainsNum := 0
		cutDomainsLen := 0
		for i := len(composedSearch) - 1; i >= 0; i-- {
			cutDomainsLen += len(composedSearch[i]) + 1
			cutDomainsNum++

			if (resolvSearchLineStrLen - cutDomainsLen) <= maxDNSSearchListChars {
				break
			}
		}

		composedSearch = composedSearch[:(len(composedSearch) - cutDomainsNum)]
		limitsExceeded = true
	}

	if limitsExceeded {
		err := fmt.Errorf("Search Line limits were exceeded, some search paths have been omitted, the applied search line is: %s", strings.Join(composedSearch, " "))
		c.recorder.Event(pod, v1.EventTypeWarning, "DNSConfigForming", err.Error())
		klog.ErrorS(err, "Search Line limits exceeded")
	}
	return composedSearch
}
// 根据nameserver最大数卡一下
func (c *Configurer) formDNSNameserversFitsLimits(nameservers []string, pod *v1.Pod) []string {
	if len(nameservers) > validation.MaxDNSNameservers {
		nameservers = nameservers[0:validation.MaxDNSNameservers]
		err := fmt.Errorf("Nameserver limits were exceeded, some nameservers have been omitted, the applied nameserver line is: %s", strings.Join(nameservers, " "))
		c.recorder.Event(pod, v1.EventTypeWarning, "DNSConfigForming", err.Error())
		klog.ErrorS(err, "Nameserver limits exceeded")
	}
	return nameservers
}
// 根据配置截断nameserver和search
func (c *Configurer) formDNSConfigFitsLimits(dnsConfig *runtimeapi.DNSConfig, pod *v1.Pod) *runtimeapi.DNSConfig {
	dnsConfig.Servers = c.formDNSNameserversFitsLimits(dnsConfig.Servers, pod)
	dnsConfig.Searches = c.formDNSSearchFitsLimits(dnsConfig.Searches, pod)
	return dnsConfig
}
// 加了几个顶级域名到search字段
func (c *Configurer) generateSearchesForDNSClusterFirst(hostSearch []string, pod *v1.Pod) []string {
	if c.ClusterDomain == "" {
		return hostSearch
	}

	nsSvcDomain := fmt.Sprintf("%s.svc.%s", pod.Namespace, c.ClusterDomain)
	svcDomain := fmt.Sprintf("svc.%s", c.ClusterDomain)
	clusterSearch := []string{nsSvcDomain, svcDomain, c.ClusterDomain}

	return omitDuplicates(append(clusterSearch, hostSearch...))
}

// CheckLimitsForResolvConf checks limits in resolv.conf.
// 检查dns的search和nameserver长度是否合法，记录到event
func (c *Configurer) CheckLimitsForResolvConf() {
	f, err := os.Open(c.ResolverConfig)
	if err != nil {
		c.recorder.Event(c.nodeRef, v1.EventTypeWarning, "CheckLimitsForResolvConf", err.Error())
		klog.V(4).InfoS("Check limits for resolv.conf failed at file open", "err", err)
		return
	}
	defer f.Close()

	_, hostSearch, _, err := parseResolvConf(f)
	if err != nil {
		c.recorder.Event(c.nodeRef, v1.EventTypeWarning, "CheckLimitsForResolvConf", err.Error())
		klog.V(4).InfoS("Check limits for resolv.conf failed at parse resolv.conf", "err", err)
		return
	}

	domainCountLimit, maxDNSSearchListChars := validation.MaxDNSSearchPathsLegacy, validation.MaxDNSSearchListCharsLegacy
	if utilfeature.DefaultFeatureGate.Enabled(features.ExpandedDNSConfig) {
		domainCountLimit, maxDNSSearchListChars = validation.MaxDNSSearchPathsExpanded, validation.MaxDNSSearchListCharsExpanded
	}

	if c.ClusterDomain != "" {
		domainCountLimit -= 3
	}

	if len(hostSearch) > domainCountLimit {
		log := fmt.Sprintf("Resolv.conf file '%s' contains search line consisting of more than %d domains!", c.ResolverConfig, domainCountLimit)
		c.recorder.Event(c.nodeRef, v1.EventTypeWarning, "CheckLimitsForResolvConf", log)
		klog.V(4).InfoS("Check limits for resolv.conf failed", "eventlog", log)
		return
	}

	for _, search := range hostSearch {
		if len(search) > utilvalidation.DNS1123SubdomainMaxLength {
			log := fmt.Sprintf("Resolv.conf file %q contains a search path which length is more than allowed %d chars!", c.ResolverConfig, utilvalidation.DNS1123SubdomainMaxLength)
			c.recorder.Event(c.nodeRef, v1.EventTypeWarning, "CheckLimitsForResolvConf", log)
			klog.V(4).InfoS("Check limits for resolv.conf failed", "eventlog", log)
			return
		}
	}

	if len(strings.Join(hostSearch, " ")) > maxDNSSearchListChars {
		log := fmt.Sprintf("Resolv.conf file '%s' contains search line which length is more than allowed %d chars!", c.ResolverConfig, maxDNSSearchListChars)
		c.recorder.Event(c.nodeRef, v1.EventTypeWarning, "CheckLimitsForResolvConf", log)
		klog.V(4).InfoS("Check limits for resolv.conf failed", "eventlog", log)
		return
	}
}

// parseResolvConf reads a resolv.conf file from the given reader, and parses
// it into nameservers, searches and options, possibly returning an error.
// 从resolv.conf文件中解析出来nameservers，searches，options
func parseResolvConf(reader io.Reader) (nameservers []string, searches []string, options []string, err error) {
	file, err := utilio.ReadAtMost(reader, maxResolveConfLength)
	if err != nil {
		return nil, nil, nil, err
	}

	// Lines of the form "nameserver 1.2.3.4" accumulate.
	nameservers = []string{}

	// Lines of the form "search example.com" overrule - last one wins.
	searches = []string{}

	// Lines of the form "option ndots:5 attempts:2" overrule - last one wins.
	// Each option is recorded as an element in the array.
	options = []string{}

	var allErrors []error
	lines := strings.Split(string(file), "\n")
	for l := range lines {
		trimmed := strings.TrimSpace(lines[l])
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "nameserver" {
			if len(fields) >= 2 {
				nameservers = append(nameservers, fields[1])
			} else {
				allErrors = append(allErrors, fmt.Errorf("nameserver list is empty "))
			}
		}
		if fields[0] == "search" {
			// Normalise search fields so the same domain with and without trailing dot will only count once, to avoid hitting search validation limits.
			searches = []string{}
			for _, s := range fields[1:] {
				searches = append(searches, strings.TrimSuffix(s, "."))
			}
		}
		if fields[0] == "options" {
			options = fields[1:]
		}
	}

	return nameservers, searches, options, utilerrors.NewAggregate(allErrors)
}
// 从 ResolverConfig文件中解析出nameservers, searches, options
func (c *Configurer) getHostDNSConfig() (*runtimeapi.DNSConfig, error) {
	var hostDNS, hostSearch, hostOptions []string
	// Get host DNS settings
	if c.ResolverConfig != "" {
		f, err := os.Open(c.ResolverConfig)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		hostDNS, hostSearch, hostOptions, err = parseResolvConf(f)
		if err != nil {
			return nil, err
		}
	}
	return &runtimeapi.DNSConfig{
		Servers:  hostDNS,
		Searches: hostSearch,
		Options:  hostOptions,
	}, nil
}
// 获取Pod上配置的DNS策略
func getPodDNSType(pod *v1.Pod) (podDNSType, error) {
	dnsPolicy := pod.Spec.DNSPolicy
	switch dnsPolicy {
	case v1.DNSNone:
		return podDNSNone, nil
	case v1.DNSClusterFirstWithHostNet:
		return podDNSCluster, nil
	case v1.DNSClusterFirst:  // 先到集群，集群没有再请求上游
		if !kubecontainer.IsHostNetworkPod(pod) {
			return podDNSCluster, nil
		}
		// Fallback to DNSDefault for pod on hostnetwork.
		fallthrough
	case v1.DNSDefault: // 直接使用宿主机的/etc/resolv.conf
		return podDNSHost, nil
	}
	// This should not happen as kube-apiserver should have rejected
	// invalid dnsPolicy.
	return podDNSCluster, fmt.Errorf(fmt.Sprintf("invalid DNSPolicy=%v", dnsPolicy))
}

// mergeDNSOptions merges DNS options. If duplicated, entries given by PodDNSConfigOption will
// overwrite the existing ones.
func mergeDNSOptions(existingDNSConfigOptions []string, dnsConfigOptions []v1.PodDNSConfigOption) []string {
	optionsMap := make(map[string]string)
	for _, op := range existingDNSConfigOptions {
		if index := strings.Index(op, ":"); index != -1 {
			optionsMap[op[:index]] = op[index+1:]
		} else {
			optionsMap[op] = ""
		}
	}
	for _, op := range dnsConfigOptions {
		if op.Value != nil {
			optionsMap[op.Name] = *op.Value
		} else {
			optionsMap[op.Name] = ""
		}
	}
	// Reconvert DNS options into a string array.
	options := []string{}
	for opName, opValue := range optionsMap {
		op := opName
		if opValue != "" {
			op = op + ":" + opValue
		}
		options = append(options, op)
	}
	return options
}

// appendDNSConfig appends DNS servers, search paths and options given by
// PodDNSConfig to the existing DNS config. Duplicated entries will be merged.
// This assumes existingDNSConfig and dnsConfig are not nil.
//  Pod上配置的DNSConfig 加入到existingDNSConfig中
func appendDNSConfig(existingDNSConfig *runtimeapi.DNSConfig, dnsConfig *v1.PodDNSConfig) *runtimeapi.DNSConfig {
	existingDNSConfig.Servers = omitDuplicates(append(existingDNSConfig.Servers, dnsConfig.Nameservers...))
	existingDNSConfig.Searches = omitDuplicates(append(existingDNSConfig.Searches, dnsConfig.Searches...))
	existingDNSConfig.Options = mergeDNSOptions(existingDNSConfig.Options, dnsConfig.Options)
	return existingDNSConfig
}

// GetPodDNS returns DNS settings for the pod.
// 返回Pod可以使用的Dns配置
func (c *Configurer) GetPodDNS(pod *v1.Pod) (*runtimeapi.DNSConfig, error) {
	// 获取resolve配置
	dnsConfig, err := c.getHostDNSConfig()
	if err != nil {
		return nil, err
	}

	dnsType, err := getPodDNSType(pod)
	if err != nil {
		klog.ErrorS(err, "Failed to get DNS type for pod. Falling back to DNSClusterFirst policy.", "pod", klog.KObj(pod))
		dnsType = podDNSCluster
	}
	switch dnsType {
	case podDNSNone: // 用空的dns配置，应该是用Pod上自己配置的
		// DNSNone should use empty DNS settings as the base.
		dnsConfig = &runtimeapi.DNSConfig{}
	case podDNSCluster: // 用集群的（集群的dns server找不到的话，回去外部的DNS server找）
		if len(c.clusterDNS) != 0 { // 集群里找
			// For a pod with DNSClusterFirst policy, the cluster DNS server is
			// the only nameserver configured for the pod. The cluster DNS server
			// itself will forward queries to other nameservers that is configured
			// to use, in case the cluster DNS server cannot resolve the DNS query
			// itself.
			dnsConfig.Servers = []string{}
			for _, ip := range c.clusterDNS {
				dnsConfig.Servers = append(dnsConfig.Servers, ip.String())
			}
			// 加了几个顶级域名(.svc.)到search字段
			dnsConfig.Searches = c.generateSearchesForDNSClusterFirst(dnsConfig.Searches, pod)
			dnsConfig.Options = defaultDNSOptions
			break
		}
		// clusterDNS is not known. Pod with ClusterDNSFirst Policy cannot be created.
		nodeErrorMsg := fmt.Sprintf("kubelet does not have ClusterDNS IP configured and cannot create Pod using %q policy. Falling back to %q policy.", v1.DNSClusterFirst, v1.DNSDefault)
		c.recorder.Eventf(c.nodeRef, v1.EventTypeWarning, "MissingClusterDNS", nodeErrorMsg)
		c.recorder.Eventf(pod, v1.EventTypeWarning, "MissingClusterDNS", "pod: %q. %s", format.Pod(pod), nodeErrorMsg)
		// Fallback to DNSDefault.
		fallthrough
	case podDNSHost: // 用本机做DNS
		// When the kubelet --resolv-conf flag is set to the empty string, use
		// DNS settings that override the docker default (which is to use
		// /etc/resolv.conf) and effectively disable DNS lookups. According to
		// the bind documentation, the behavior of the DNS client library when
		// "nameservers" are not specified is to "use the nameserver on the
		// local machine". A nameserver setting of localhost is equivalent to
		// this documented behavior.
		if c.ResolverConfig == "" { // 看起来是用自己作为DNS
			for _, nodeIP := range c.nodeIPs {
				if utilnet.IsIPv6(nodeIP) {
					dnsConfig.Servers = append(dnsConfig.Servers, "::1")
				} else {
					dnsConfig.Servers = append(dnsConfig.Servers, "127.0.0.1")
				}
			}
			if len(dnsConfig.Servers) == 0 {
				dnsConfig.Servers = append(dnsConfig.Servers, "127.0.0.1")
			}
			dnsConfig.Searches = []string{"."}
		}
	}

	if pod.Spec.DNSConfig != nil {  // Pod上的DNS配置append进去
		dnsConfig = appendDNSConfig(dnsConfig, pod.Spec.DNSConfig)
	}
	return c.formDNSConfigFitsLimits(dnsConfig, pod), nil
}

// SetupDNSinContainerizedMounter replaces the nameserver in containerized-mounter's rootfs/etc/resolve.conf with kubelet.ClusterDNS
// 根据clusterDNS生成nameserver指令，根据ResolverConfig生成search指令，最后写到rootfs/etc/resolve.conf中
func (c *Configurer) SetupDNSinContainerizedMounter(mounterPath string) {
	resolvePath := filepath.Join(strings.TrimSuffix(mounterPath, "/mounter"), "rootfs", "etc", "resolv.conf")
	dnsString := ""
	for _, dns := range c.clusterDNS { // 拼接nameserver命令
		dnsString = dnsString + fmt.Sprintf("nameserver %s\n", dns)
	}
	if c.ResolverConfig != "" {
		f, err := os.Open(c.ResolverConfig)
		if err != nil {
			klog.ErrorS(err, "Could not open resolverConf file")
		} else {
			defer f.Close()
			_, hostSearch, _, err := parseResolvConf(f)
			if err != nil {
				klog.ErrorS(err, "Error for parsing the resolv.conf file")
			} else {
				dnsString = dnsString + "search"
				for _, search := range hostSearch {// 拼接search命令
					dnsString = dnsString + fmt.Sprintf(" %s", search)
				}
				dnsString = dnsString + "\n"
			}
		}
	}
	if err := ioutil.WriteFile(resolvePath, []byte(dnsString), 0600); err != nil {
		klog.ErrorS(err, "Could not write dns nameserver in the file", "path", resolvePath)
	}
}
