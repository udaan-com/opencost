package costmodel

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kubecost/opencost/pkg/util/timeutil"

	"github.com/kubecost/opencost/pkg/cloud"
	"github.com/kubecost/opencost/pkg/env"
	"github.com/kubecost/opencost/pkg/kubecost"
	"github.com/kubecost/opencost/pkg/log"
	"github.com/kubecost/opencost/pkg/prom"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	queryFmtPods                     = `avg(kube_pod_container_status_running{clutser="udaan-k8s0"}) by (pod, namespace, %s)[%s:%s]`
	queryFmtPodsUID                  = `avg(kube_pod_container_status_running{clutser="udaan-k8s0"}) by (pod, namespace, uid, %s)[%s:%s]`
	queryFmtRAMBytesAllocated        = `avg(avg_over_time(container_memory_allocation_bytes{container!="", container!="POD", node!="", clutser="udaan-k8s0"}[%s])) by (container, pod, namespace, node, %s, provider_id)`
	queryFmtRAMRequests              = `avg(avg_over_time(kube_pod_container_resource_requests{resource="memory", unit="byte", container!="", container!="POD", node!="", clutser="udaan-k8s0"}[%s])) by (container, pod, namespace, node, %s)`
	queryFmtRAMUsageAvg              = `avg(avg_over_time(container_memory_working_set_bytes{container!="", container_name!="POD", container!="POD", clutser="udaan-k8s0"}[%s])) by (container_name, container, pod_name, pod, namespace, instance, %s)`
	queryFmtRAMUsageMax              = `max(max_over_time(container_memory_working_set_bytes{container!="", container_name!="POD", container!="POD", clutser="udaan-k8s0"}[%s])) by (container_name, container, pod_name, pod, namespace, instance, %s)`
	queryFmtCPUCoresAllocated        = `avg(avg_over_time(container_cpu_allocation{container!="", container!="POD", node!="", clutser="udaan-k8s0"}[%s])) by (container, pod, namespace, node, %s)`
	queryFmtCPURequests              = `avg(avg_over_time(kube_pod_container_resource_requests{resource="cpu", unit="core", container!="", container!="POD", node!="", clutser="udaan-k8s0"}[%s])) by (container, pod, namespace, node, %s)`
	queryFmtCPUUsageAvg              = `avg(rate(container_cpu_usage_seconds_total{container!="", container_name!="POD", container!="POD", clutser="udaan-k8s0"}[%s])) by (container_name, container, pod_name, pod, namespace, instance, %s)`
	queryFmtCPUUsageMax              = `max(rate(container_cpu_usage_seconds_total{container!="", container_name!="POD", container!="POD", clutser="udaan-k8s0"}[%s])) by (container_name, container, pod_name, pod, namespace, instance, %s)`
	queryFmtGPUsRequested            = `avg(avg_over_time(kube_pod_container_resource_requests{resource="nvidia_com_gpu", container!="",container!="POD", node!="", clutser="udaan-k8s0"}[%s])) by (container, pod, namespace, node, %s)`
	queryFmtGPUsAllocated            = `avg(avg_over_time(container_gpu_allocation{container!="", container!="POD", node!="", clutser="udaan-k8s0"}[%s])) by (container, pod, namespace, node, %s)`
	queryFmtNodeCostPerCPUHr         = `avg(avg_over_time(node_cpu_hourly_cost{clutser="udaan-k8s0"}[%s])) by (node, %s, instance_type, provider_id)`
	queryFmtNodeCostPerRAMGiBHr      = `avg(avg_over_time(node_ram_hourly_cost{clutser="udaan-k8s0"}[%s])) by (node, %s, instance_type, provider_id)`
	queryFmtNodeCostPerGPUHr         = `avg(avg_over_time(node_gpu_hourly_cost{clutser="udaan-k8s0"}[%s])) by (node, %s, instance_type, provider_id)`
	queryFmtNodeIsSpot               = `avg_over_time(kubecost_node_is_spot{clutser="udaan-k8s0"}[%s])`
	queryFmtPVCInfo                  = `avg(kube_persistentvolumeclaim_info{volumename != "", clutser="udaan-k8s0"}) by (persistentvolumeclaim, storageclass, volumename, namespace, %s)[%s:%s]`
	queryFmtPVBytes                  = `avg(avg_over_time(kube_persistentvolume_capacity_bytes{clutser="udaan-k8s0"}[%s])) by (persistentvolume, %s)`
	queryFmtPodPVCAllocation         = `avg(avg_over_time(pod_pvc_allocation{clutser="udaan-k8s0"}[%s])) by (persistentvolume, persistentvolumeclaim, pod, namespace, %s)`
	queryFmtPVCBytesRequested        = `avg(avg_over_time(kube_persistentvolumeclaim_resource_requests_storage_bytes{clutser="udaan-k8s0"}[%s])) by (persistentvolumeclaim, namespace, %s)`
	queryFmtPVCostPerGiBHour         = `avg(avg_over_time(pv_hourly_cost{clutser="udaan-k8s0"}[%s])) by (volumename, %s)`
	queryFmtNetZoneGiB               = `sum(increase(kubecost_pod_network_egress_bytes_total{internet="false", sameZone="false", sameRegion="true", clutser="udaan-k8s0"}[%s])) by (pod_name, namespace, %s) / 1024 / 1024 / 1024`
	queryFmtNetZoneCostPerGiB        = `avg(avg_over_time(kubecost_network_zone_egress_cost{clutser="udaan-k8s0"}[%s])) by (%s)`
	queryFmtNetRegionGiB             = `sum(increase(kubecost_pod_network_egress_bytes_total{internet="false", sameZone="false", sameRegion="false", clutser="udaan-k8s0"}[%s])) by (pod_name, namespace, %s) / 1024 / 1024 / 1024`
	queryFmtNetRegionCostPerGiB      = `avg(avg_over_time(kubecost_network_region_egress_cost{clutser="udaan-k8s0"}[%s])) by (%s)`
	queryFmtNetInternetGiB           = `sum(increase(kubecost_pod_network_egress_bytes_total{internet="true", clutser="udaan-k8s0"}[%s])) by (pod_name, namespace, %s) / 1024 / 1024 / 1024`
	queryFmtNetInternetCostPerGiB    = `avg(avg_over_time(kubecost_network_internet_egress_cost{clutser="udaan-k8s0"}[%s])) by (%s)`
	queryFmtNetReceiveBytes          = `sum(increase(container_network_receive_bytes_total{pod!="", container="POD", clutser="udaan-k8s0"}[%s])) by (pod_name, pod, namespace, %s)`
	queryFmtNetTransferBytes         = `sum(increase(container_network_transmit_bytes_total{pod!="", container="POD", clutser="udaan-k8s0"}[%s])) by (pod_name, pod, namespace, %s)`
	queryFmtNamespaceLabels          = `avg_over_time(kube_namespace_labels{clutser="udaan-k8s0"}[%s])`
	queryFmtNamespaceAnnotations     = `avg_over_time(kube_namespace_annotations{clutser="udaan-k8s0"}[%s])`
	queryFmtPodLabels                = `avg_over_time(kube_pod_labels{clutser="udaan-k8s0"}[%s])`
	queryFmtPodAnnotations           = `avg_over_time(kube_pod_annotations{clutser="udaan-k8s0"}[%s])`
	queryFmtServiceLabels            = `avg_over_time(service_selector_labels{clutser="udaan-k8s0"}[%s])`
	queryFmtDeploymentLabels         = `avg_over_time(deployment_match_labels{clutser="udaan-k8s0"}[%s])`
	queryFmtStatefulSetLabels        = `avg_over_time(statefulSet_match_labels{clutser="udaan-k8s0"}[%s])`
	queryFmtDaemonSetLabels          = `sum(avg_over_time(kube_pod_owner{owner_kind="DaemonSet", clutser="udaan-k8s0"}[%s])) by (pod, owner_name, namespace, %s)`
	queryFmtJobLabels                = `sum(avg_over_time(kube_pod_owner{owner_kind="Job", clutser="udaan-k8s0"}[%s])) by (pod, owner_name, namespace ,%s)`
	queryFmtPodsWithReplicaSetOwner  = `sum(avg_over_time(kube_pod_owner{owner_kind="ReplicaSet", clutser="udaan-k8s0"}[%s])) by (pod, owner_name, namespace ,%s)`
	queryFmtReplicaSetsWithoutOwners = `avg(avg_over_time(kube_replicaset_owner{owner_kind="<none>", owner_name="<none>", clutser="udaan-k8s0"}[%s])) by (replicaset, namespace, %s)`
	queryFmtLBCostPerHr              = `avg(avg_over_time(kubecost_load_balancer_cost{clutser="udaan-k8s0"}[%s])) by (namespace, service_name, %s)`
	queryFmtLBActiveMins             = `count(kubecost_load_balancer_cost{clutser="udaan-k8s0"}) by (namespace, service_name, %s)[%s:%s]`
)

// This is a bit of a hack to work around garbage data from cadvisor
// Ideally you cap each pod to the max CPU on its node, but that involves a bit more complexity, as it it would need to be done when allocations joins with asset data.
const MAX_CPU_CAP = 512

// CanCompute should return true if CostModel can act as a valid source for the
// given time range. In the case of CostModel we want to attempt to compute as
// long as the range starts in the past. If the CostModel ends up not having
// data to match, that's okay, and should be communicated with an error
// response from ComputeAllocation.
func (cm *CostModel) CanCompute(start, end time.Time) bool {
	return start.Before(time.Now())
}

// Name returns the name of the Source
func (cm *CostModel) Name() string {
	return "CostModel"
}

// ComputeAllocation uses the CostModel instance to compute an AllocationSet
// for the window defined by the given start and end times. The Allocations
// returned are unaggregated (i.e. down to the container level).
func (cm *CostModel) ComputeAllocation(start, end time.Time, resolution time.Duration) (*kubecost.AllocationSet, error) {
	// If the duration is short enough, compute the AllocationSet directly
	if end.Sub(start) <= cm.MaxPrometheusQueryDuration {
		return cm.computeAllocation(start, end, resolution)
	}

	// If the duration exceeds the configured MaxPrometheusQueryDuration, then
	// query for maximum-sized AllocationSets, collect them, and accumulate.

	// s and e track the coverage of the entire given window over multiple
	// internal queries.
	s, e := start, start

	// Collect AllocationSets in a range, then accumulate
	// TODO optimize by collecting consecutive AllocationSets, accumulating as we go
	asr := kubecost.NewAllocationSetRange()

	for e.Before(end) {
		// By default, query for the full remaining duration. But do not let
		// any individual query duration exceed the configured max Prometheus
		// query duration.
		duration := end.Sub(e)
		if duration > cm.MaxPrometheusQueryDuration {
			duration = cm.MaxPrometheusQueryDuration
		}

		// Set start and end parameters (s, e) for next individual computation.
		e = s.Add(duration)

		// Compute the individual AllocationSet for just (s, e)
		as, err := cm.computeAllocation(s, e, resolution)
		if err != nil {
			return kubecost.NewAllocationSet(start, end), fmt.Errorf("error computing allocation for %s: %s", kubecost.NewClosedWindow(s, e), err)
		}

		// Append to the range
		asr.Append(as)

		// Set s equal to e to set up the next query, if one exists.
		s = e
	}

	// Populate annotations, labels, and services on each Allocation. This is
	// necessary because Properties.Intersection does not propagate any values
	// stored in maps or slices for performance reasons. In this case, however,
	// it is both acceptable and necessary to do so.
	allocationAnnotations := map[string]map[string]string{}
	allocationLabels := map[string]map[string]string{}
	allocationServices := map[string]map[string]bool{}

	// Also record errors and warnings, then append them to the results later.
	errors := []string{}
	warnings := []string{}

	asr.Each(func(i int, as *kubecost.AllocationSet) {
		as.Each(func(k string, a *kubecost.Allocation) {
			if len(a.Properties.Annotations) > 0 {
				if _, ok := allocationAnnotations[k]; !ok {
					allocationAnnotations[k] = map[string]string{}
				}
				for name, val := range a.Properties.Annotations {
					allocationAnnotations[k][name] = val
				}
			}

			if len(a.Properties.Labels) > 0 {
				if _, ok := allocationLabels[k]; !ok {
					allocationLabels[k] = map[string]string{}
				}
				for name, val := range a.Properties.Labels {
					allocationLabels[k][name] = val
				}
			}

			if len(a.Properties.Services) > 0 {
				if _, ok := allocationServices[k]; !ok {
					allocationServices[k] = map[string]bool{}
				}
				for _, val := range a.Properties.Services {
					allocationServices[k][val] = true
				}
			}
		})

		errors = append(errors, as.Errors...)
		warnings = append(warnings, as.Warnings...)
	})

	// Accumulate to yield the result AllocationSet. After this step, we will
	// be nearly complete, but without the raw allocation data, which must be
	// recomputed.
	result, err := asr.Accumulate()
	if err != nil {
		return kubecost.NewAllocationSet(start, end), fmt.Errorf("error accumulating data for %s: %s", kubecost.NewClosedWindow(s, e), err)
	}

	// Apply the annotations, labels, and services to the post-accumulation
	// results. (See above for why this is necessary.)
	result.Each(func(k string, a *kubecost.Allocation) {
		if annotations, ok := allocationAnnotations[k]; ok {
			a.Properties.Annotations = annotations
		}

		if labels, ok := allocationLabels[k]; ok {
			a.Properties.Labels = labels
		}

		if services, ok := allocationServices[k]; ok {
			a.Properties.Services = []string{}
			for s := range services {
				a.Properties.Services = append(a.Properties.Services, s)
			}
		}

		// Expand the Window of all Allocations within the AllocationSet
		// to match the Window of the AllocationSet, which gets expanded
		// at the end of this function.
		a.Window = a.Window.ExpandStart(start).ExpandEnd(end)
	})

	// Maintain RAM and CPU max usage values by iterating over the range,
	// computing maximums on a rolling basis, and setting on the result set.
	asr.Each(func(i int, as *kubecost.AllocationSet) {
		as.Each(func(key string, alloc *kubecost.Allocation) {
			resultAlloc := result.Get(key)
			if resultAlloc == nil {
				return
			}

			if resultAlloc.RawAllocationOnly == nil {
				resultAlloc.RawAllocationOnly = &kubecost.RawAllocationOnlyData{}
			}

			if alloc.RawAllocationOnly == nil {
				// This will happen inevitably for unmounted disks, but should
				// ideally not happen for any allocation with CPU and RAM data.
				if !alloc.IsUnmounted() {
					log.DedupedWarningf(10, "ComputeAllocation: raw allocation data missing for %s", key)
				}
				return
			}

			if alloc.RawAllocationOnly.CPUCoreUsageMax > resultAlloc.RawAllocationOnly.CPUCoreUsageMax {
				resultAlloc.RawAllocationOnly.CPUCoreUsageMax = alloc.RawAllocationOnly.CPUCoreUsageMax
			}

			if alloc.RawAllocationOnly.RAMBytesUsageMax > resultAlloc.RawAllocationOnly.RAMBytesUsageMax {
				resultAlloc.RawAllocationOnly.RAMBytesUsageMax = alloc.RawAllocationOnly.RAMBytesUsageMax
			}
		})
	})

	// Expand the window to match the queried time range.
	result.Window = result.Window.ExpandStart(start).ExpandEnd(end)

	// Append errors and warnings
	result.Errors = errors
	result.Warnings = warnings

	return result, nil
}

func (cm *CostModel) computeAllocation(start, end time.Time, resolution time.Duration) (*kubecost.AllocationSet, error) {
	// 1. Build out Pod map from resolution-tuned, batched Pod start/end query
	// 2. Run and apply the results of the remaining queries to
	// 3. Build out AllocationSet from completed Pod map

	// Create a window spanning the requested query
	window := kubecost.NewWindow(&start, &end)

	// Create an empty AllocationSet. For safety, in the case of an error, we
	// should prefer to return this empty set with the error. (In the case of
	// no error, of course we populate the set and return it.)
	allocSet := kubecost.NewAllocationSet(start, end)

	// (1) Build out Pod map

	// Build out a map of Allocations as a mapping from pod-to-container-to-
	// underlying-Allocation instance, starting with (start, end) so that we
	// begin with minutes, from which we compute resource allocation and cost
	// totals from measured rate data.
	podMap := map[podKey]*Pod{}

	// clusterStarts and clusterEnds record the earliest start and latest end
	// times, respectively, on a cluster-basis. These are used for unmounted
	// PVs and other "virtual" Allocations so that minutes are maximally
	// accurate during start-up or spin-down of a cluster
	clusterStart := map[string]time.Time{}
	clusterEnd := map[string]time.Time{}

	// If ingesting pod UID, we query kube_pod_container_status_running avg
	// by uid as well as the default values, and all podKeys/pods have their
	// names changed to "<pod_name> <pod_uid>". Because other metrics need
	// to generate keys to match pods but don't have UIDs, podUIDKeyMap
	// stores values of format:

	// default podKey : []{edited podkey 1, edited podkey 2}

	// This is because ingesting UID allows us to catch uncontrolled pods
	// with the same names. However, this will lead to a many-to-one metric
	// to podKey relation, so this map allows us to map the metric's
	// "<pod_name>" key to the edited "<pod_name> <pod_uid>" keys in podMap.
	ingestPodUID := env.IsIngestingPodUID()
	podUIDKeyMap := make(map[podKey][]podKey)

	if ingestPodUID {
		log.Debugf("CostModel.ComputeAllocation: ingesting UID data from KSM metrics...")
	}

	// TODO:CLEANUP remove "max batch" idea and clusterStart/End
	cm.buildPodMap(window, resolution, env.GetETLMaxPrometheusQueryDuration(), podMap, clusterStart, clusterEnd, ingestPodUID, podUIDKeyMap)

	// (2) Run and apply remaining queries

	// Query for the duration between start and end
	durStr := timeutil.DurationString(end.Sub(start))
	if durStr == "" {
		return allocSet, fmt.Errorf("illegal duration value for %s", kubecost.NewClosedWindow(start, end))
	}

	// Convert resolution duration to a query-ready string
	resStr := timeutil.DurationString(resolution)

	ctx := prom.NewNamedContext(cm.PrometheusClient, prom.AllocationContextName)

	queryRAMBytesAllocated := fmt.Sprintf(queryFmtRAMBytesAllocated, durStr, env.GetPromClusterLabel())
	resChRAMBytesAllocated := ctx.QueryAtTime(queryRAMBytesAllocated, end)

	queryRAMRequests := fmt.Sprintf(queryFmtRAMRequests, durStr, env.GetPromClusterLabel())
	resChRAMRequests := ctx.QueryAtTime(queryRAMRequests, end)

	queryRAMUsageAvg := fmt.Sprintf(queryFmtRAMUsageAvg, durStr, env.GetPromClusterLabel())
	resChRAMUsageAvg := ctx.QueryAtTime(queryRAMUsageAvg, end)

	queryRAMUsageMax := fmt.Sprintf(queryFmtRAMUsageMax, durStr, env.GetPromClusterLabel())
	resChRAMUsageMax := ctx.QueryAtTime(queryRAMUsageMax, end)

	queryCPUCoresAllocated := fmt.Sprintf(queryFmtCPUCoresAllocated, durStr, env.GetPromClusterLabel())
	resChCPUCoresAllocated := ctx.QueryAtTime(queryCPUCoresAllocated, end)

	queryCPURequests := fmt.Sprintf(queryFmtCPURequests, durStr, env.GetPromClusterLabel())
	resChCPURequests := ctx.QueryAtTime(queryCPURequests, end)

	queryCPUUsageAvg := fmt.Sprintf(queryFmtCPUUsageAvg, durStr, env.GetPromClusterLabel())
	resChCPUUsageAvg := ctx.QueryAtTime(queryCPUUsageAvg, end)

	queryCPUUsageMax := fmt.Sprintf(queryFmtCPUUsageMax, durStr, env.GetPromClusterLabel())
	resChCPUUsageMax := ctx.QueryAtTime(queryCPUUsageMax, end)

	queryGPUsRequested := fmt.Sprintf(queryFmtGPUsRequested, durStr, env.GetPromClusterLabel())
	resChGPUsRequested := ctx.QueryAtTime(queryGPUsRequested, end)

	queryGPUsAllocated := fmt.Sprintf(queryFmtGPUsAllocated, durStr, env.GetPromClusterLabel())
	resChGPUsAllocated := ctx.QueryAtTime(queryGPUsAllocated, end)

	queryNodeCostPerCPUHr := fmt.Sprintf(queryFmtNodeCostPerCPUHr, durStr, env.GetPromClusterLabel())
	resChNodeCostPerCPUHr := ctx.QueryAtTime(queryNodeCostPerCPUHr, end)

	queryNodeCostPerRAMGiBHr := fmt.Sprintf(queryFmtNodeCostPerRAMGiBHr, durStr, env.GetPromClusterLabel())
	resChNodeCostPerRAMGiBHr := ctx.QueryAtTime(queryNodeCostPerRAMGiBHr, end)

	queryNodeCostPerGPUHr := fmt.Sprintf(queryFmtNodeCostPerGPUHr, durStr, env.GetPromClusterLabel())
	resChNodeCostPerGPUHr := ctx.QueryAtTime(queryNodeCostPerGPUHr, end)

	queryNodeIsSpot := fmt.Sprintf(queryFmtNodeIsSpot, durStr)
	resChNodeIsSpot := ctx.QueryAtTime(queryNodeIsSpot, end)

	queryPVCInfo := fmt.Sprintf(queryFmtPVCInfo, env.GetPromClusterLabel(), durStr, resStr)
	resChPVCInfo := ctx.QueryAtTime(queryPVCInfo, end)

	queryPVBytes := fmt.Sprintf(queryFmtPVBytes, durStr, env.GetPromClusterLabel())
	resChPVBytes := ctx.QueryAtTime(queryPVBytes, end)

	queryPodPVCAllocation := fmt.Sprintf(queryFmtPodPVCAllocation, durStr, env.GetPromClusterLabel())
	resChPodPVCAllocation := ctx.QueryAtTime(queryPodPVCAllocation, end)

	queryPVCBytesRequested := fmt.Sprintf(queryFmtPVCBytesRequested, durStr, env.GetPromClusterLabel())
	resChPVCBytesRequested := ctx.QueryAtTime(queryPVCBytesRequested, end)

	queryPVCostPerGiBHour := fmt.Sprintf(queryFmtPVCostPerGiBHour, durStr, env.GetPromClusterLabel())
	resChPVCostPerGiBHour := ctx.QueryAtTime(queryPVCostPerGiBHour, end)

	queryNetTransferBytes := fmt.Sprintf(queryFmtNetTransferBytes, durStr, env.GetPromClusterLabel())
	resChNetTransferBytes := ctx.QueryAtTime(queryNetTransferBytes, end)

	queryNetReceiveBytes := fmt.Sprintf(queryFmtNetReceiveBytes, durStr, env.GetPromClusterLabel())
	resChNetReceiveBytes := ctx.QueryAtTime(queryNetReceiveBytes, end)

	queryNetZoneGiB := fmt.Sprintf(queryFmtNetZoneGiB, durStr, env.GetPromClusterLabel())
	resChNetZoneGiB := ctx.QueryAtTime(queryNetZoneGiB, end)

	queryNetZoneCostPerGiB := fmt.Sprintf(queryFmtNetZoneCostPerGiB, durStr, env.GetPromClusterLabel())
	resChNetZoneCostPerGiB := ctx.QueryAtTime(queryNetZoneCostPerGiB, end)

	queryNetRegionGiB := fmt.Sprintf(queryFmtNetRegionGiB, durStr, env.GetPromClusterLabel())
	resChNetRegionGiB := ctx.QueryAtTime(queryNetRegionGiB, end)

	queryNetRegionCostPerGiB := fmt.Sprintf(queryFmtNetRegionCostPerGiB, durStr, env.GetPromClusterLabel())
	resChNetRegionCostPerGiB := ctx.QueryAtTime(queryNetRegionCostPerGiB, end)

	queryNetInternetGiB := fmt.Sprintf(queryFmtNetInternetGiB, durStr, env.GetPromClusterLabel())
	resChNetInternetGiB := ctx.QueryAtTime(queryNetInternetGiB, end)

	queryNetInternetCostPerGiB := fmt.Sprintf(queryFmtNetInternetCostPerGiB, durStr, env.GetPromClusterLabel())
	resChNetInternetCostPerGiB := ctx.QueryAtTime(queryNetInternetCostPerGiB, end)

	queryNamespaceLabels := fmt.Sprintf(queryFmtNamespaceLabels, durStr)
	resChNamespaceLabels := ctx.QueryAtTime(queryNamespaceLabels, end)

	queryNamespaceAnnotations := fmt.Sprintf(queryFmtNamespaceAnnotations, durStr)
	resChNamespaceAnnotations := ctx.QueryAtTime(queryNamespaceAnnotations, end)

	queryPodLabels := fmt.Sprintf(queryFmtPodLabels, durStr)
	resChPodLabels := ctx.QueryAtTime(queryPodLabels, end)

	queryPodAnnotations := fmt.Sprintf(queryFmtPodAnnotations, durStr)
	resChPodAnnotations := ctx.QueryAtTime(queryPodAnnotations, end)

	queryServiceLabels := fmt.Sprintf(queryFmtServiceLabels, durStr)
	resChServiceLabels := ctx.QueryAtTime(queryServiceLabels, end)

	queryDeploymentLabels := fmt.Sprintf(queryFmtDeploymentLabels, durStr)
	resChDeploymentLabels := ctx.QueryAtTime(queryDeploymentLabels, end)

	queryStatefulSetLabels := fmt.Sprintf(queryFmtStatefulSetLabels, durStr)
	resChStatefulSetLabels := ctx.QueryAtTime(queryStatefulSetLabels, end)

	queryDaemonSetLabels := fmt.Sprintf(queryFmtDaemonSetLabels, durStr, env.GetPromClusterLabel())
	resChDaemonSetLabels := ctx.QueryAtTime(queryDaemonSetLabels, end)

	queryPodsWithReplicaSetOwner := fmt.Sprintf(queryFmtPodsWithReplicaSetOwner, durStr, env.GetPromClusterLabel())
	resChPodsWithReplicaSetOwner := ctx.QueryAtTime(queryPodsWithReplicaSetOwner, end)

	queryReplicaSetsWithoutOwners := fmt.Sprintf(queryFmtReplicaSetsWithoutOwners, durStr, env.GetPromClusterLabel())
	resChReplicaSetsWithoutOwners := ctx.QueryAtTime(queryReplicaSetsWithoutOwners, end)

	queryJobLabels := fmt.Sprintf(queryFmtJobLabels, durStr, env.GetPromClusterLabel())
	resChJobLabels := ctx.QueryAtTime(queryJobLabels, end)

	queryLBCostPerHr := fmt.Sprintf(queryFmtLBCostPerHr, durStr, env.GetPromClusterLabel())
	resChLBCostPerHr := ctx.QueryAtTime(queryLBCostPerHr, end)

	queryLBActiveMins := fmt.Sprintf(queryFmtLBActiveMins, env.GetPromClusterLabel(), durStr, resStr)
	resChLBActiveMins := ctx.QueryAtTime(queryLBActiveMins, end)

	resCPUCoresAllocated, _ := resChCPUCoresAllocated.Await()
	resCPURequests, _ := resChCPURequests.Await()
	resCPUUsageAvg, _ := resChCPUUsageAvg.Await()
	resCPUUsageMax, _ := resChCPUUsageMax.Await()
	resRAMBytesAllocated, _ := resChRAMBytesAllocated.Await()
	resRAMRequests, _ := resChRAMRequests.Await()
	resRAMUsageAvg, _ := resChRAMUsageAvg.Await()
	resRAMUsageMax, _ := resChRAMUsageMax.Await()
	resGPUsRequested, _ := resChGPUsRequested.Await()
	resGPUsAllocated, _ := resChGPUsAllocated.Await()

	resNodeCostPerCPUHr, _ := resChNodeCostPerCPUHr.Await()
	resNodeCostPerRAMGiBHr, _ := resChNodeCostPerRAMGiBHr.Await()
	resNodeCostPerGPUHr, _ := resChNodeCostPerGPUHr.Await()
	resNodeIsSpot, _ := resChNodeIsSpot.Await()

	resPVBytes, _ := resChPVBytes.Await()
	resPVCostPerGiBHour, _ := resChPVCostPerGiBHour.Await()

	resPVCInfo, _ := resChPVCInfo.Await()
	resPVCBytesRequested, _ := resChPVCBytesRequested.Await()
	resPodPVCAllocation, _ := resChPodPVCAllocation.Await()

	resNetTransferBytes, _ := resChNetTransferBytes.Await()
	resNetReceiveBytes, _ := resChNetReceiveBytes.Await()
	resNetZoneGiB, _ := resChNetZoneGiB.Await()
	resNetZoneCostPerGiB, _ := resChNetZoneCostPerGiB.Await()
	resNetRegionGiB, _ := resChNetRegionGiB.Await()
	resNetRegionCostPerGiB, _ := resChNetRegionCostPerGiB.Await()
	resNetInternetGiB, _ := resChNetInternetGiB.Await()
	resNetInternetCostPerGiB, _ := resChNetInternetCostPerGiB.Await()

	resNamespaceLabels, _ := resChNamespaceLabels.Await()
	resNamespaceAnnotations, _ := resChNamespaceAnnotations.Await()
	resPodLabels, _ := resChPodLabels.Await()
	resPodAnnotations, _ := resChPodAnnotations.Await()
	resServiceLabels, _ := resChServiceLabels.Await()
	resDeploymentLabels, _ := resChDeploymentLabels.Await()
	resStatefulSetLabels, _ := resChStatefulSetLabels.Await()
	resDaemonSetLabels, _ := resChDaemonSetLabels.Await()
	resPodsWithReplicaSetOwner, _ := resChPodsWithReplicaSetOwner.Await()
	resReplicaSetsWithoutOwners, _ := resChReplicaSetsWithoutOwners.Await()
	resJobLabels, _ := resChJobLabels.Await()
	resLBCostPerHr, _ := resChLBCostPerHr.Await()
	resLBActiveMins, _ := resChLBActiveMins.Await()

	if ctx.HasErrors() {
		for _, err := range ctx.Errors() {
			log.Errorf("CostModel.ComputeAllocation: %s", err)
		}

		return allocSet, ctx.ErrorCollection()
	}

	// We choose to apply allocation before requests in the cases of RAM and
	// CPU so that we can assert that allocation should always be greater than
	// or equal to request.
	applyCPUCoresAllocated(podMap, resCPUCoresAllocated, podUIDKeyMap)
	applyCPUCoresRequested(podMap, resCPURequests, podUIDKeyMap)
	applyCPUCoresUsedAvg(podMap, resCPUUsageAvg, podUIDKeyMap)
	applyCPUCoresUsedMax(podMap, resCPUUsageMax, podUIDKeyMap)
	applyRAMBytesAllocated(podMap, resRAMBytesAllocated, podUIDKeyMap)
	applyRAMBytesRequested(podMap, resRAMRequests, podUIDKeyMap)
	applyRAMBytesUsedAvg(podMap, resRAMUsageAvg, podUIDKeyMap)
	applyRAMBytesUsedMax(podMap, resRAMUsageMax, podUIDKeyMap)
	applyGPUsAllocated(podMap, resGPUsRequested, resGPUsAllocated, podUIDKeyMap)
	applyNetworkTotals(podMap, resNetTransferBytes, resNetReceiveBytes, podUIDKeyMap)
	applyNetworkAllocation(podMap, resNetZoneGiB, resNetZoneCostPerGiB, podUIDKeyMap)
	applyNetworkAllocation(podMap, resNetRegionGiB, resNetRegionCostPerGiB, podUIDKeyMap)
	applyNetworkAllocation(podMap, resNetInternetGiB, resNetInternetCostPerGiB, podUIDKeyMap)

	// In the case that a two pods with the same name had different containers,
	// we will double-count the containers. There is no way to associate each
	// container with the proper pod from the usage metrics above. This will
	// show up as a pod having two Allocations running for the whole pod runtime.

	// Other than that case, Allocations should be associated with pods by the
	// above functions.

	namespaceLabels := resToNamespaceLabels(resNamespaceLabels)
	podLabels := resToPodLabels(resPodLabels, podUIDKeyMap, ingestPodUID)
	namespaceAnnotations := resToNamespaceAnnotations(resNamespaceAnnotations)
	podAnnotations := resToPodAnnotations(resPodAnnotations, podUIDKeyMap, ingestPodUID)
	applyLabels(podMap, namespaceLabels, podLabels)
	applyAnnotations(podMap, namespaceAnnotations, podAnnotations)

	serviceLabels := getServiceLabels(resServiceLabels)
	allocsByService := map[serviceKey][]*kubecost.Allocation{}
	applyServicesToPods(podMap, podLabels, allocsByService, serviceLabels)

	podDeploymentMap := labelsToPodControllerMap(podLabels, resToDeploymentLabels(resDeploymentLabels))
	podStatefulSetMap := labelsToPodControllerMap(podLabels, resToStatefulSetLabels(resStatefulSetLabels))
	podDaemonSetMap := resToPodDaemonSetMap(resDaemonSetLabels, podUIDKeyMap, ingestPodUID)
	podJobMap := resToPodJobMap(resJobLabels, podUIDKeyMap, ingestPodUID)
	podReplicaSetMap := resToPodReplicaSetMap(resPodsWithReplicaSetOwner, resReplicaSetsWithoutOwners, podUIDKeyMap, ingestPodUID)
	applyControllersToPods(podMap, podDeploymentMap)
	applyControllersToPods(podMap, podStatefulSetMap)
	applyControllersToPods(podMap, podDaemonSetMap)
	applyControllersToPods(podMap, podJobMap)
	applyControllersToPods(podMap, podReplicaSetMap)

	// TODO breakdown network costs?

	// Build out a map of Nodes with resource costs, discounts, and node types
	// for converting resource allocation data to cumulative costs.
	nodeMap := map[nodeKey]*NodePricing{}

	applyNodeCostPerCPUHr(nodeMap, resNodeCostPerCPUHr)
	applyNodeCostPerRAMGiBHr(nodeMap, resNodeCostPerRAMGiBHr)
	applyNodeCostPerGPUHr(nodeMap, resNodeCostPerGPUHr)
	applyNodeSpot(nodeMap, resNodeIsSpot)
	applyNodeDiscount(nodeMap, cm)

	// Build out the map of all PVs with class, size and cost-per-hour.
	// Note: this does not record time running, which we may want to
	// include later for increased PV precision. (As long as the PV has
	// a PVC, we get time running there, so this is only inaccurate
	// for short-lived, unmounted PVs.)
	pvMap := map[pvKey]*PV{}
	buildPVMap(pvMap, resPVCostPerGiBHour)
	applyPVBytes(pvMap, resPVBytes)

	// Build out the map of all PVCs with time running, bytes requested,
	// and connect to the correct PV from pvMap. (If no PV exists, that
	// is noted, but does not result in any allocation/cost.)
	pvcMap := map[pvcKey]*PVC{}
	buildPVCMap(window, pvcMap, pvMap, resPVCInfo)
	applyPVCBytesRequested(pvcMap, resPVCBytesRequested)

	// Build out the relationships of pods to their PVCs. This step
	// populates the PVC.Count field so that PVC allocation can be
	// split appropriately among each pod's container allocation.
	podPVCMap := map[podKey][]*PVC{}
	buildPodPVCMap(podPVCMap, pvMap, pvcMap, podMap, resPodPVCAllocation, podUIDKeyMap, ingestPodUID)

	// Because PVCs can be shared among pods, the respective PV cost
	// needs to be evenly distributed to those pods based on time
	// running, as well as the amount of time the PVC was shared.

	// Build a relation between every PVC to the pods that mount it
	// and a window representing the interval during which they
	// were associated.
	pvcPodIntervalMap := make(map[pvcKey]map[podKey]kubecost.Window)

	for _, pod := range podMap {

		for _, alloc := range pod.Allocations {

			cluster := alloc.Properties.Cluster
			namespace := alloc.Properties.Namespace
			pod := alloc.Properties.Pod
			thisPodKey := newPodKey(cluster, namespace, pod)

			if pvcs, ok := podPVCMap[thisPodKey]; ok {
				for _, pvc := range pvcs {

					// Determine the (start, end) of the relationship between the
					// given PVC and the associated Allocation so that a precise
					// number of hours can be used to compute cumulative cost.
					s, e := alloc.Start, alloc.End
					if pvc.Start.After(alloc.Start) {
						s = pvc.Start
					}
					if pvc.End.Before(alloc.End) {
						e = pvc.End
					}

					thisPVCKey := newPVCKey(cluster, namespace, pvc.Name)
					if pvcPodIntervalMap[thisPVCKey] == nil {
						pvcPodIntervalMap[thisPVCKey] = make(map[podKey]kubecost.Window)
					}

					pvcPodIntervalMap[thisPVCKey][thisPodKey] = kubecost.NewWindow(&s, &e)
				}
			}

			// We only need to look at one alloc per pod
			break
		}

	}

	// Build out a PV price coefficient for each pod with a PVC. Each
	// PVC-pod relation needs a coefficient which modifies the PV cost
	// such that PV costs can be shared between all pods using that PVC.
	sharedPVCCostCoefficientMap := make(map[pvcKey]map[podKey][]CoefficientComponent)
	for pvcKey, podIntervalMap := range pvcPodIntervalMap {

		// Get single-point intervals from alloc-PVC relation windows.
		intervals := getIntervalPointsFromWindows(podIntervalMap)

		// Determine coefficients for each PVC-pod relation.
		sharedPVCCostCoefficientMap[pvcKey] = getPVCCostCoefficients(intervals, podIntervalMap)

	}

	// Identify unmounted PVs (PVs without PVCs) and add one Allocation per
	// cluster representing each cluster's unmounted PVs (if necessary).
	applyUnmountedPVs(window, podMap, pvMap, pvcMap)

	lbMap := getLoadBalancerCosts(resLBCostPerHr, resLBActiveMins, resolution)
	applyLoadBalancersToPods(lbMap, allocsByService)

	// (3) Build out AllocationSet from Pod map

	for _, pod := range podMap {
		for _, alloc := range pod.Allocations {
			cluster := alloc.Properties.Cluster
			nodeName := alloc.Properties.Node
			namespace := alloc.Properties.Namespace
			pod := alloc.Properties.Pod
			container := alloc.Properties.Container

			podKey := newPodKey(cluster, namespace, pod)
			nodeKey := newNodeKey(cluster, nodeName)

			node := cm.getNodePricing(nodeMap, nodeKey)
			alloc.Properties.ProviderID = node.ProviderID
			alloc.CPUCost = alloc.CPUCoreHours * node.CostPerCPUHr
			alloc.RAMCost = (alloc.RAMByteHours / 1024 / 1024 / 1024) * node.CostPerRAMGiBHr
			alloc.GPUCost = alloc.GPUHours * node.CostPerGPUHr

			if pvcs, ok := podPVCMap[podKey]; ok {
				for _, pvc := range pvcs {

					pvcKey := newPVCKey(cluster, namespace, pvc.Name)

					s, e := alloc.Start, alloc.End
					if pvcInterval, ok := pvcPodIntervalMap[pvcKey][podKey]; ok {
						s, e = *pvcInterval.Start(), *pvcInterval.End()
					} else {
						log.Warnf("CostModel.ComputeAllocation: allocation %s and PVC %s have no associated active window", alloc.Name, pvc.Name)
					}

					minutes := e.Sub(s).Minutes()
					hrs := minutes / 60.0

					count := float64(pvc.Count)
					if pvc.Count < 1 {
						count = 1
					}

					gib := pvc.Bytes / 1024 / 1024 / 1024
					cost := pvc.Volume.CostPerGiBHour * gib * hrs

					// Scale PV cost by PVC sharing coefficient.
					if coeffComponents, ok := sharedPVCCostCoefficientMap[pvcKey][podKey]; ok {
						cost *= getCoefficientFromComponents(coeffComponents)
					} else {
						log.Warnf("CostModel.ComputeAllocation: allocation %s and PVC %s have relation but no coeff", alloc.Name, pvc.Name)
					}

					// Apply the size and cost of the PV to the allocation, each
					// weighted by count (i.e. the number of containers in the pod)
					// record the amount of total PVBytes Hours attributable to a given PV
					if alloc.PVs == nil {
						alloc.PVs = kubecost.PVAllocations{}
					}
					pvKey := kubecost.PVKey{
						Cluster: pvc.Cluster,
						Name:    pvc.Volume.Name,
					}
					alloc.PVs[pvKey] = &kubecost.PVAllocation{
						ByteHours: pvc.Bytes * hrs / count,
						Cost:      cost / count,
					}
				}
			}

			// Make sure that the name is correct (node may not be present at this
			// point due to it missing from queryMinutes) then insert.
			alloc.Name = fmt.Sprintf("%s/%s/%s/%s/%s", cluster, nodeName, namespace, pod, container)
			allocSet.Set(alloc)
		}
	}

	return allocSet, nil
}

func (cm *CostModel) buildPodMap(window kubecost.Window, resolution, maxBatchSize time.Duration, podMap map[podKey]*Pod, clusterStart, clusterEnd map[string]time.Time, ingestPodUID bool, podUIDKeyMap map[podKey][]podKey) error {
	// Assumes that window is positive and closed
	start, end := *window.Start(), *window.End()

	// Convert resolution duration to a query-ready string
	resStr := timeutil.DurationString(resolution)

	ctx := prom.NewNamedContext(cm.PrometheusClient, prom.AllocationContextName)

	// Query for (start, end) by (pod, namespace, cluster) over the given
	// window, using the given resolution, and if necessary in batches no
	// larger than the given maximum batch size. If working in batches, track
	// overall progress by starting with (window.start, window.start) and
	// querying in batches no larger than maxBatchSize from start-to-end,
	// folding each result set into podMap as the results come back.
	coverage := kubecost.NewWindow(&start, &start)

	numQuery := 1
	for coverage.End().Before(end) {
		// Determine the (start, end) of the current batch
		batchStart := *coverage.End()
		batchEnd := coverage.End().Add(maxBatchSize)
		if batchEnd.After(end) {
			batchEnd = end
		}

		var resPods []*prom.QueryResult
		var err error
		maxTries := 3
		numTries := 0
		for resPods == nil && numTries < maxTries {
			numTries++

			// Query for the duration between start and end
			durStr := timeutil.DurationString(batchEnd.Sub(batchStart))
			if durStr == "" {
				// Negative duration, so set empty results and don't query
				resPods = []*prom.QueryResult{}
				err = nil
				break
			}

			// Submit and profile query

			var queryPods string
			// If ingesting UIDs, avg on them
			if ingestPodUID {
				queryPods = fmt.Sprintf(queryFmtPodsUID, env.GetPromClusterLabel(), durStr, resStr)
			} else {
				queryPods = fmt.Sprintf(queryFmtPods, env.GetPromClusterLabel(), durStr, resStr)
			}

			queryProfile := time.Now()
			resPods, err = ctx.QueryAtTime(queryPods, batchEnd).Await()
			if err != nil {
				log.Profile(queryProfile, fmt.Sprintf("CostModel.ComputeAllocation: pod query %d try %d failed: %s", numQuery, numTries, queryPods))
				resPods = nil
			}
		}

		if err != nil {
			return err
		}

		// queryFmtPodsUID will return both UID-containing results, and non-UID-containing results,
		// so filter out the non-containing results so we don't duplicate pods. This is due to the
		// default setup of Kubecost having replicated kube_pod_container_status_running and
		// included KSM kube_pod_container_status_running. Querying w/ UID will return both.
		if ingestPodUID {
			var resPodsUID []*prom.QueryResult

			for _, res := range resPods {
				_, err := res.GetString("uid")
				if err == nil {
					resPodsUID = append(resPodsUID, res)
				}
			}

			if len(resPodsUID) > 0 {
				resPods = resPodsUID
			} else {
				log.DedupedWarningf(5, "CostModel.ComputeAllocation: UID ingestion enabled, but query did not return any results with UID")
			}
		}

		applyPodResults(window, resolution, podMap, clusterStart, clusterEnd, resPods, ingestPodUID, podUIDKeyMap)

		coverage = coverage.ExpandEnd(batchEnd)
		numQuery++
	}

	return nil
}

func applyPodResults(window kubecost.Window, resolution time.Duration, podMap map[podKey]*Pod, clusterStart, clusterEnd map[string]time.Time, resPods []*prom.QueryResult, ingestPodUID bool, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resPods {
		if len(res.Values) == 0 {
			log.Warnf("CostModel.ComputeAllocation: empty minutes result")
			continue
		}

		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		labels, err := res.GetStrings("namespace", "pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: minutes query result missing field: %s", err)
			continue
		}

		namespace := labels["namespace"]
		pod := labels["pod"]
		key := newPodKey(cluster, namespace, pod)

		// If pod UIDs are being used to ID pods, append them to the pod name in
		// the podKey.
		if ingestPodUID {

			uid, err := res.GetString("uid")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: UID ingestion enabled, but query result missing field: %s", err)
			} else {

				newKey := newPodKey(cluster, namespace, pod+" "+uid)
				podUIDKeyMap[key] = append(podUIDKeyMap[key], newKey)

				key = newKey

			}

		}

		// allocStart and allocEnd are the timestamps of the first and last
		// minutes the pod was running, respectively. We subtract one resolution
		// from allocStart because this point will actually represent the end
		// of the first minute. We don't subtract from allocEnd because it
		// already represents the end of the last minute.
		var allocStart, allocEnd time.Time
		startAdjustmentCoeff, endAdjustmentCoeff := 1.0, 1.0
		for _, datum := range res.Values {
			t := time.Unix(int64(datum.Timestamp), 0)

			if allocStart.IsZero() && datum.Value > 0 && window.Contains(t) {
				// Set the start timestamp to the earliest non-zero timestamp
				allocStart = t

				// Record adjustment coefficient, i.e. the portion of the start
				// timestamp to "ignore". That is, sometimes the value will be
				// 0.5, meaning that we should discount the time running by
				// half of the resolution the timestamp stands for.
				startAdjustmentCoeff = (1.0 - datum.Value)
			}

			if datum.Value > 0 && window.Contains(t) {
				// Set the end timestamp to the latest non-zero timestamp
				allocEnd = t

				// Record adjustment coefficient, i.e. the portion of the end
				// timestamp to "ignore". (See explanation above for start.)
				endAdjustmentCoeff = (1.0 - datum.Value)
			}
		}

		if allocStart.IsZero() || allocEnd.IsZero() {
			continue
		}

		// Adjust timestamps according to the resolution and the adjustment
		// coefficients, as described above. That is, count the start timestamp
		// from the beginning of the resolution, not the end. Then "reduce" the
		// start and end by the correct amount, in the case that the "running"
		// value of the first or last timestamp was not a full 1.0.
		allocStart = allocStart.Add(-resolution)
		// Note: the *100 and /100 are necessary because Duration is an int, so
		// 0.5, for instance, will be truncated, resulting in no adjustment.
		allocStart = allocStart.Add(time.Duration(startAdjustmentCoeff*100) * resolution / time.Duration(100))
		allocEnd = allocEnd.Add(-time.Duration(endAdjustmentCoeff*100) * resolution / time.Duration(100))

		// Ensure that the allocStart is always within the window, adjusting
		// for the occasions where start falls 1m before the query window.
		// NOTE: window here will always be closed (so no need to nil check
		// "start").
		// TODO:CLEANUP revisit query methodology to figure out why this is
		// happening on occasion
		if allocStart.Before(*window.Start()) {
			allocStart = *window.Start()
		}

		// If there is only one point with a value <= 0.5 that the start and
		// end timestamps both share, then we will enter this case because at
		// least half of a resolution will be subtracted from both the start
		// and the end. If that is the case, then add back half of each side
		// so that the pod is said to run for half a resolution total.
		// e.g. For resolution 1m and a value of 0.5 at one timestamp, we'll
		//      end up with allocEnd == allocStart and each coeff == 0.5. In
		//      that case, add 0.25m to each side, resulting in 0.5m duration.
		if !allocEnd.After(allocStart) {
			allocStart = allocStart.Add(-time.Duration(50*startAdjustmentCoeff) * resolution / time.Duration(100))
			allocEnd = allocEnd.Add(time.Duration(50*endAdjustmentCoeff) * resolution / time.Duration(100))
		}

		// Ensure that the allocEnf is always within the window, adjusting
		// for the occasions where end falls 1m after the query window. This
		// has not ever happened, but is symmetrical with the start check
		// above.
		// NOTE: window here will always be closed (so no need to nil check
		// "end").
		// TODO:CLEANUP revisit query methodology to figure out why this is
		// happening on occasion
		if allocEnd.After(*window.End()) {
			allocEnd = *window.End()
		}

		// Set start if unset or this datum's start time is earlier than the
		// current earliest time.
		if _, ok := clusterStart[cluster]; !ok || allocStart.Before(clusterStart[cluster]) {
			clusterStart[cluster] = allocStart
		}

		// Set end if unset or this datum's end time is later than the
		// current latest time.
		if _, ok := clusterEnd[cluster]; !ok || allocEnd.After(clusterEnd[cluster]) {
			clusterEnd[cluster] = allocEnd
		}

		if pod, ok := podMap[key]; ok {
			// Pod has already been recorded, so update it accordingly
			if allocStart.Before(pod.Start) {
				pod.Start = allocStart
			}
			if allocEnd.After(pod.End) {
				pod.End = allocEnd
			}
		} else {
			// Pod has not been recorded yet, so insert it
			podMap[key] = &Pod{
				Window:      window.Clone(),
				Start:       allocStart,
				End:         allocEnd,
				Key:         key,
				Allocations: map[string]*kubecost.Allocation{},
			}
		}
	}
}

func applyCPUCoresAllocated(podMap map[podKey]*Pod, resCPUCoresAllocated []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresAllocated {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU allocation result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU allocation query result missing 'container': %s", key)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			cpuCores := res.Values[0].Value
			if cpuCores > MAX_CPU_CAP {
				log.Infof("[WARNING] Very large cpu allocation, clamping to %f", res.Values[0].Value*(pod.Allocations[container].Minutes()/60.0))
				cpuCores = 0.0
			}
			hours := pod.Allocations[container].Minutes() / 60.0
			pod.Allocations[container].CPUCoreHours = cpuCores * hours

			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: CPU allocation query result missing 'node': %s", key)
				continue
			}
			pod.Allocations[container].Properties.Node = node
		}
	}
}

func applyCPUCoresRequested(podMap map[podKey]*Pod, resCPUCoresRequested []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresRequested {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU request result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU request query result missing 'container': %s", key)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			pod.Allocations[container].CPUCoreRequestAverage = res.Values[0].Value

			// If CPU allocation is less than requests, set CPUCoreHours to
			// request level.
			if pod.Allocations[container].CPUCores() < res.Values[0].Value {
				pod.Allocations[container].CPUCoreHours = res.Values[0].Value * (pod.Allocations[container].Minutes() / 60.0)
			}
			if pod.Allocations[container].CPUCores() > MAX_CPU_CAP {
				log.Infof("[WARNING] Very large cpu allocation, clamping! to %f", res.Values[0].Value*(pod.Allocations[container].Minutes()/60.0))
				pod.Allocations[container].CPUCoreHours = res.Values[0].Value * (pod.Allocations[container].Minutes() / 60.0)
			}
			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: CPU request query result missing 'node': %s", key)
				continue
			}
			pod.Allocations[container].Properties.Node = node
		}
	}
}

func applyCPUCoresUsedAvg(podMap map[podKey]*Pod, resCPUCoresUsedAvg []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresUsedAvg {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage avg result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage avg query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			pod.Allocations[container].CPUCoreUsageAverage = res.Values[0].Value
			if res.Values[0].Value > MAX_CPU_CAP {
				log.Infof("[WARNING] Very large cpu USAGE, dropping outlier")
				pod.Allocations[container].CPUCoreUsageAverage = 0.0
			}
		}
	}
}

func applyCPUCoresUsedMax(podMap map[podKey]*Pod, resCPUCoresUsedMax []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resCPUCoresUsedMax {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage max result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: CPU usage max query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			if pod.Allocations[container].RawAllocationOnly == nil {
				pod.Allocations[container].RawAllocationOnly = &kubecost.RawAllocationOnlyData{
					CPUCoreUsageMax: res.Values[0].Value,
				}
			} else {
				pod.Allocations[container].RawAllocationOnly.CPUCoreUsageMax = res.Values[0].Value
			}
		}
	}
}

func applyRAMBytesAllocated(podMap map[podKey]*Pod, resRAMBytesAllocated []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesAllocated {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM allocation result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM allocation query result missing 'container': %s", key)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			ramBytes := res.Values[0].Value
			hours := pod.Allocations[container].Minutes() / 60.0
			pod.Allocations[container].RAMByteHours = ramBytes * hours

			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: RAM allocation query result missing 'node': %s", key)
				continue
			}
			pod.Allocations[container].Properties.Node = node
		}
	}
}

func applyRAMBytesRequested(podMap map[podKey]*Pod, resRAMBytesRequested []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesRequested {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM request result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM request query result missing 'container': %s", key)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			pod.Allocations[container].RAMBytesRequestAverage = res.Values[0].Value

			// If RAM allocation is less than requests, set RAMByteHours to
			// request level.
			if pod.Allocations[container].RAMBytes() < res.Values[0].Value {
				pod.Allocations[container].RAMByteHours = res.Values[0].Value * (pod.Allocations[container].Minutes() / 60.0)
			}

			node, err := res.GetString("node")
			if err != nil {
				log.Warnf("CostModel.ComputeAllocation: RAM request query result missing 'node': %s", key)
				continue
			}
			pod.Allocations[container].Properties.Node = node
		}
	}
}

func applyRAMBytesUsedAvg(podMap map[podKey]*Pod, resRAMBytesUsedAvg []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesUsedAvg {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM avg usage result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM usage avg query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			pod.Allocations[container].RAMBytesUsageAverage = res.Values[0].Value
		}
	}
}

func applyRAMBytesUsedMax(podMap map[podKey]*Pod, resRAMBytesUsedMax []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resRAMBytesUsedMax {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM usage max result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if container == "" || err != nil {
			container, err = res.GetString("container_name")
			if err != nil {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: RAM usage max query result missing 'container': %s", key)
				continue
			}
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			if pod.Allocations[container].RawAllocationOnly == nil {
				pod.Allocations[container].RawAllocationOnly = &kubecost.RawAllocationOnlyData{
					RAMBytesUsageMax: res.Values[0].Value,
				}
			} else {
				pod.Allocations[container].RawAllocationOnly.RAMBytesUsageMax = res.Values[0].Value
			}
		}
	}
}

func applyGPUsAllocated(podMap map[podKey]*Pod, resGPUsRequested []*prom.QueryResult, resGPUsAllocated []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	if len(resGPUsAllocated) > 0 { // Use the new query, when it's become available in a window
		resGPUsRequested = resGPUsAllocated
	}
	for _, res := range resGPUsRequested {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: GPU request result missing field: %s", err)
			continue
		}

		container, err := res.GetString("container")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: GPU request query result missing 'container': %s", key)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[key]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[key]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {

			if _, ok := pod.Allocations[container]; !ok {
				pod.AppendContainer(container)
			}

			hrs := pod.Allocations[container].Minutes() / 60.0
			pod.Allocations[container].GPUHours = res.Values[0].Value * hrs
		}
	}
}

func applyNetworkTotals(podMap map[podKey]*Pod, resNetworkTransferBytes []*prom.QueryResult, resNetworkReceiveBytes []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	for _, res := range resNetworkTransferBytes {
		podKey, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: Network Transfer Bytes query result missing field: %s", err)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[podKey]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[podKey]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {
			for _, alloc := range pod.Allocations {
				alloc.NetworkTransferBytes = res.Values[0].Value / float64(len(pod.Allocations)) / float64(len(pods))
			}
		}
	}
	for _, res := range resNetworkReceiveBytes {
		podKey, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: Network Receive Bytes query result missing field: %s", err)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[podKey]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[podKey]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {
			for _, alloc := range pod.Allocations {
				alloc.NetworkReceiveBytes = res.Values[0].Value / float64(len(pod.Allocations)) / float64(len(pods))
			}
		}
	}
}

func applyNetworkAllocation(podMap map[podKey]*Pod, resNetworkGiB []*prom.QueryResult, resNetworkCostPerGiB []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey) {
	costPerGiBByCluster := map[string]float64{}

	for _, res := range resNetworkCostPerGiB {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		costPerGiBByCluster[cluster] = res.Values[0].Value
	}

	for _, res := range resNetworkGiB {
		podKey, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: Network allocation query result missing field: %s", err)
			continue
		}

		var pods []*Pod

		pod, ok := podMap[podKey]
		if !ok {
			if uidKeys, ok := podUIDKeyMap[podKey]; ok {
				for _, uidKey := range uidKeys {
					pod, ok = podMap[uidKey]
					if ok {
						pods = append(pods, pod)
					}
				}
			} else {
				continue
			}
		} else {
			pods = []*Pod{pod}
		}

		for _, pod := range pods {
			for _, alloc := range pod.Allocations {
				gib := res.Values[0].Value / float64(len(pod.Allocations))
				costPerGiB := costPerGiBByCluster[podKey.Cluster]
				alloc.NetworkCost = gib * costPerGiB / float64(len(pods))
			}
		}
	}
}

func resToNamespaceLabels(resNamespaceLabels []*prom.QueryResult) map[namespaceKey]map[string]string {
	namespaceLabels := map[namespaceKey]map[string]string{}

	for _, res := range resNamespaceLabels {
		nsKey, err := resultNamespaceKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			continue
		}

		if _, ok := namespaceLabels[nsKey]; !ok {
			namespaceLabels[nsKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			namespaceLabels[nsKey][k] = l
		}
	}

	return namespaceLabels
}

func resToPodLabels(resPodLabels []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]map[string]string {
	podLabels := map[podKey]map[string]string{}

	for _, res := range resPodLabels {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			continue
		}

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			if _, ok := podLabels[key]; !ok {
				podLabels[key] = map[string]string{}
			}

			for k, l := range res.GetLabels() {
				podLabels[key][k] = l
			}
		}
	}

	return podLabels
}

func resToNamespaceAnnotations(resNamespaceAnnotations []*prom.QueryResult) map[string]map[string]string {
	namespaceAnnotations := map[string]map[string]string{}

	for _, res := range resNamespaceAnnotations {
		namespace, err := res.GetString("namespace")
		if err != nil {
			continue
		}

		if _, ok := namespaceAnnotations[namespace]; !ok {
			namespaceAnnotations[namespace] = map[string]string{}
		}

		for k, l := range res.GetAnnotations() {
			namespaceAnnotations[namespace][k] = l
		}
	}

	return namespaceAnnotations
}

func resToPodAnnotations(resPodAnnotations []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]map[string]string {
	podAnnotations := map[podKey]map[string]string{}

	for _, res := range resPodAnnotations {
		key, err := resultPodKey(res, env.GetPromClusterLabel(), "namespace")
		if err != nil {
			continue
		}

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			if _, ok := podAnnotations[key]; !ok {
				podAnnotations[key] = map[string]string{}
			}

			for k, l := range res.GetAnnotations() {
				podAnnotations[key][k] = l
			}
		}
	}

	return podAnnotations
}

func applyLabels(podMap map[podKey]*Pod, namespaceLabels map[namespaceKey]map[string]string, podLabels map[podKey]map[string]string) {
	for podKey, pod := range podMap {
		for _, alloc := range pod.Allocations {
			allocLabels := alloc.Properties.Labels
			if allocLabels == nil {
				allocLabels = make(map[string]string)
			}
			// Apply namespace labels first, then pod labels so that pod labels
			// overwrite namespace labels.
			nsKey := podKey.namespaceKey // newNamespaceKey(podKey.Cluster, podKey.Namespace)
			if labels, ok := namespaceLabels[nsKey]; ok {
				for k, v := range labels {
					allocLabels[k] = v
				}
			}
			if labels, ok := podLabels[podKey]; ok {
				for k, v := range labels {
					allocLabels[k] = v
				}
			}

			alloc.Properties.Labels = allocLabels
		}
	}
}

func applyAnnotations(podMap map[podKey]*Pod, namespaceAnnotations map[string]map[string]string, podAnnotations map[podKey]map[string]string) {
	for key, pod := range podMap {
		for _, alloc := range pod.Allocations {
			allocAnnotations := alloc.Properties.Annotations
			if allocAnnotations == nil {
				allocAnnotations = make(map[string]string)
			}
			// Apply namespace annotations first, then pod annotations so that
			// pod labels overwrite namespace labels.
			if labels, ok := namespaceAnnotations[key.Namespace]; ok {
				for k, v := range labels {
					allocAnnotations[k] = v
				}
			}
			if labels, ok := podAnnotations[key]; ok {
				for k, v := range labels {
					allocAnnotations[k] = v
				}
			}

			alloc.Properties.Annotations = allocAnnotations
		}
	}
}

func getServiceLabels(resServiceLabels []*prom.QueryResult) map[serviceKey]map[string]string {
	serviceLabels := map[serviceKey]map[string]string{}

	for _, res := range resServiceLabels {
		serviceKey, err := resultServiceKey(res, env.GetPromClusterLabel(), "namespace", "service")
		if err != nil {
			continue
		}

		if _, ok := serviceLabels[serviceKey]; !ok {
			serviceLabels[serviceKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			serviceLabels[serviceKey][k] = l
		}
	}

	// Prune duplicate services. That is, if the same service exists with
	// hyphens instead of underscores, keep the one that uses hyphens.
	for key := range serviceLabels {
		if strings.Contains(key.Service, "_") {
			duplicateService := strings.Replace(key.Service, "_", "-", -1)
			duplicateKey := newServiceKey(key.Cluster, key.Namespace, duplicateService)
			if _, ok := serviceLabels[duplicateKey]; ok {
				delete(serviceLabels, key)
			}
		}
	}

	return serviceLabels
}

func resToDeploymentLabels(resDeploymentLabels []*prom.QueryResult) map[controllerKey]map[string]string {
	deploymentLabels := map[controllerKey]map[string]string{}

	for _, res := range resDeploymentLabels {
		controllerKey, err := resultDeploymentKey(res, env.GetPromClusterLabel(), "namespace", "deployment")
		if err != nil {
			continue
		}

		if _, ok := deploymentLabels[controllerKey]; !ok {
			deploymentLabels[controllerKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			deploymentLabels[controllerKey][k] = l
		}
	}

	// Prune duplicate deployments. That is, if the same deployment exists with
	// hyphens instead of underscores, keep the one that uses hyphens.
	for key := range deploymentLabels {
		if strings.Contains(key.Controller, "_") {
			duplicateController := strings.Replace(key.Controller, "_", "-", -1)
			duplicateKey := newControllerKey(key.Cluster, key.Namespace, key.ControllerKind, duplicateController)
			if _, ok := deploymentLabels[duplicateKey]; ok {
				delete(deploymentLabels, key)
			}
		}
	}

	return deploymentLabels
}

func resToStatefulSetLabels(resStatefulSetLabels []*prom.QueryResult) map[controllerKey]map[string]string {
	statefulSetLabels := map[controllerKey]map[string]string{}

	for _, res := range resStatefulSetLabels {
		controllerKey, err := resultStatefulSetKey(res, env.GetPromClusterLabel(), "namespace", "statefulSet")
		if err != nil {
			continue
		}

		if _, ok := statefulSetLabels[controllerKey]; !ok {
			statefulSetLabels[controllerKey] = map[string]string{}
		}

		for k, l := range res.GetLabels() {
			statefulSetLabels[controllerKey][k] = l
		}
	}

	// Prune duplicate stateful sets. That is, if the same stateful set exists
	// with hyphens instead of underscores, keep the one that uses hyphens.
	for key := range statefulSetLabels {
		if strings.Contains(key.Controller, "_") {
			duplicateController := strings.Replace(key.Controller, "_", "-", -1)
			duplicateKey := newControllerKey(key.Cluster, key.Namespace, key.ControllerKind, duplicateController)
			if _, ok := statefulSetLabels[duplicateKey]; ok {
				delete(statefulSetLabels, key)
			}
		}
	}

	return statefulSetLabels
}

func labelsToPodControllerMap(podLabels map[podKey]map[string]string, controllerLabels map[controllerKey]map[string]string) map[podKey]controllerKey {
	podControllerMap := map[podKey]controllerKey{}

	// For each controller, turn the labels into a selector and attempt to
	// match it with each set of pod labels. A match indicates that the pod
	// belongs to the controller.
	for cKey, cLabels := range controllerLabels {
		selector := labels.Set(cLabels).AsSelectorPreValidated()

		for pKey, pLabels := range podLabels {
			// If the pod is in a different cluster or namespace, there is
			// no need to compare the labels.
			if cKey.Cluster != pKey.Cluster || cKey.Namespace != pKey.Namespace {
				continue
			}

			podLabelSet := labels.Set(pLabels)
			if selector.Matches(podLabelSet) {
				if _, ok := podControllerMap[pKey]; ok {
					log.DedupedWarningf(5, "CostModel.ComputeAllocation: PodControllerMap match already exists: %s matches %s and %s", pKey, podControllerMap[pKey], cKey)
				}
				podControllerMap[pKey] = cKey
			}
		}
	}

	return podControllerMap
}

func resToPodDaemonSetMap(resDaemonSetLabels []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]controllerKey {
	daemonSetLabels := map[podKey]controllerKey{}

	for _, res := range resDaemonSetLabels {
		controllerKey, err := resultDaemonSetKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
		if err != nil {
			continue
		}

		pod, err := res.GetString("pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: DaemonSetLabel result without pod: %s", controllerKey)
		}

		key := newPodKey(controllerKey.Cluster, controllerKey.Namespace, pod)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			daemonSetLabels[key] = controllerKey
		}
	}

	return daemonSetLabels
}

func resToPodJobMap(resJobLabels []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]controllerKey {
	jobLabels := map[podKey]controllerKey{}

	for _, res := range resJobLabels {
		controllerKey, err := resultJobKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
		if err != nil {
			continue
		}

		// Convert the name of Jobs generated by CronJobs to the name of the
		// CronJob by stripping the timestamp off the end.
		match := isCron.FindStringSubmatch(controllerKey.Controller)
		if match != nil {
			controllerKey.Controller = match[1]
		}

		pod, err := res.GetString("pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: JobLabel result without pod: %s", controllerKey)
		}

		key := newPodKey(controllerKey.Cluster, controllerKey.Namespace, pod)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {
			jobLabels[key] = controllerKey
		}
	}

	return jobLabels
}

func resToPodReplicaSetMap(resPodsWithReplicaSetOwner []*prom.QueryResult, resReplicaSetsWithoutOwners []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) map[podKey]controllerKey {
	// Build out set of ReplicaSets that have no owners, themselves, such that
	// the ReplicaSet should be used as the owner of the Pods it controls.
	// (This should exclude, for example, ReplicaSets that are controlled by
	// Deployments, in which case the Deployment should be the Pod's owner.)
	replicaSets := map[controllerKey]struct{}{}

	for _, res := range resReplicaSetsWithoutOwners {
		controllerKey, err := resultReplicaSetKey(res, env.GetPromClusterLabel(), "namespace", "replicaset")
		if err != nil {
			continue
		}

		replicaSets[controllerKey] = struct{}{}
	}

	// Create the mapping of Pods to ReplicaSets, ignoring any ReplicaSets that
	// to not appear in the set of uncontrolled ReplicaSets above.
	podToReplicaSet := map[podKey]controllerKey{}

	for _, res := range resPodsWithReplicaSetOwner {
		controllerKey, err := resultReplicaSetKey(res, env.GetPromClusterLabel(), "namespace", "owner_name")
		if err != nil {
			continue
		}
		if _, ok := replicaSets[controllerKey]; !ok {
			continue
		}

		pod, err := res.GetString("pod")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: ReplicaSet result without pod: %s", controllerKey)
		}

		key := newPodKey(controllerKey.Cluster, controllerKey.Namespace, pod)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {

			podToReplicaSet[key] = controllerKey

		}
	}

	return podToReplicaSet
}

func applyServicesToPods(podMap map[podKey]*Pod, podLabels map[podKey]map[string]string, allocsByService map[serviceKey][]*kubecost.Allocation, serviceLabels map[serviceKey]map[string]string) {
	podServicesMap := map[podKey][]serviceKey{}

	// For each service, turn the labels into a selector and attempt to
	// match it with each set of pod labels. A match indicates that the pod
	// belongs to the service.
	for sKey, sLabels := range serviceLabels {
		selector := labels.Set(sLabels).AsSelectorPreValidated()

		for pKey, pLabels := range podLabels {
			// If the pod is in a different cluster or namespace, there is
			// no need to compare the labels.
			if sKey.Cluster != pKey.Cluster || sKey.Namespace != pKey.Namespace {
				continue
			}

			podLabelSet := labels.Set(pLabels)
			if selector.Matches(podLabelSet) {
				if _, ok := podServicesMap[pKey]; !ok {
					podServicesMap[pKey] = []serviceKey{}
				}
				podServicesMap[pKey] = append(podServicesMap[pKey], sKey)
			}
		}
	}

	// For each allocation in each pod, attempt to find and apply the list of
	// services associated with the allocation's pod.
	for key, pod := range podMap {
		for _, alloc := range pod.Allocations {
			if sKeys, ok := podServicesMap[key]; ok {
				services := []string{}
				for _, sKey := range sKeys {
					services = append(services, sKey.Service)
					allocsByService[sKey] = append(allocsByService[sKey], alloc)
				}
				alloc.Properties.Services = services

			}
		}
	}
}

func applyControllersToPods(podMap map[podKey]*Pod, podControllerMap map[podKey]controllerKey) {
	for key, pod := range podMap {
		for _, alloc := range pod.Allocations {
			if controllerKey, ok := podControllerMap[key]; ok {
				alloc.Properties.ControllerKind = controllerKey.ControllerKind
				alloc.Properties.Controller = controllerKey.Controller
			}
		}
	}
}

func applyNodeCostPerCPUHr(nodeMap map[nodeKey]*NodePricing, resNodeCostPerCPUHr []*prom.QueryResult) {
	for _, res := range resNodeCostPerCPUHr {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node CPU cost query result missing field: %s", err)
			continue
		}

		instanceType, err := res.GetString("instance_type")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node CPU cost query result missing field: %s", err)
			continue
		}

		providerID, err := res.GetString("provider_id")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node CPU cost query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			nodeMap[key] = &NodePricing{
				Name:       node,
				NodeType:   instanceType,
				ProviderID: cloud.ParseID(providerID),
			}
		}

		nodeMap[key].CostPerCPUHr = res.Values[0].Value
	}
}

func applyNodeCostPerRAMGiBHr(nodeMap map[nodeKey]*NodePricing, resNodeCostPerRAMGiBHr []*prom.QueryResult) {
	for _, res := range resNodeCostPerRAMGiBHr {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node RAM cost query result missing field: %s", err)
			continue
		}

		instanceType, err := res.GetString("instance_type")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node RAM cost query result missing field: %s", err)
			continue
		}

		providerID, err := res.GetString("provider_id")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node RAM cost query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			nodeMap[key] = &NodePricing{
				Name:       node,
				NodeType:   instanceType,
				ProviderID: cloud.ParseID(providerID),
			}
		}

		nodeMap[key].CostPerRAMGiBHr = res.Values[0].Value
	}
}

func applyNodeCostPerGPUHr(nodeMap map[nodeKey]*NodePricing, resNodeCostPerGPUHr []*prom.QueryResult) {
	for _, res := range resNodeCostPerGPUHr {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node GPU cost query result missing field: %s", err)
			continue
		}

		instanceType, err := res.GetString("instance_type")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node GPU cost query result missing field: %s", err)
			continue
		}

		providerID, err := res.GetString("provider_id")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node GPU cost query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			nodeMap[key] = &NodePricing{
				Name:       node,
				NodeType:   instanceType,
				ProviderID: cloud.ParseID(providerID),
			}
		}

		nodeMap[key].CostPerGPUHr = res.Values[0].Value
	}
}

func applyNodeSpot(nodeMap map[nodeKey]*NodePricing, resNodeIsSpot []*prom.QueryResult) {
	for _, res := range resNodeIsSpot {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		node, err := res.GetString("node")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: Node spot query result missing field: %s", err)
			continue
		}

		key := newNodeKey(cluster, node)
		if _, ok := nodeMap[key]; !ok {
			log.Warnf("CostModel.ComputeAllocation: Node spot  query result for missing node: %s", key)
			continue
		}

		nodeMap[key].Preemptible = res.Values[0].Value > 0
	}
}

func applyNodeDiscount(nodeMap map[nodeKey]*NodePricing, cm *CostModel) {
	if cm == nil {
		return
	}

	c, err := cm.Provider.GetConfig()
	if err != nil {
		log.Errorf("CostModel.ComputeAllocation: applyNodeDiscount: %s", err)
		return
	}

	discount, err := ParsePercentString(c.Discount)
	if err != nil {
		log.Errorf("CostModel.ComputeAllocation: applyNodeDiscount: %s", err)
		return
	}

	negotiatedDiscount, err := ParsePercentString(c.NegotiatedDiscount)
	if err != nil {
		log.Errorf("CostModel.ComputeAllocation: applyNodeDiscount: %s", err)
		return
	}

	for _, node := range nodeMap {
		// TODO GKE Reserved Instances into account
		node.Discount = cm.Provider.CombinedDiscountForNode(node.NodeType, node.Preemptible, discount, negotiatedDiscount)
		node.CostPerCPUHr *= (1.0 - node.Discount)
		node.CostPerRAMGiBHr *= (1.0 - node.Discount)
	}
}

func buildPVMap(pvMap map[pvKey]*PV, resPVCostPerGiBHour []*prom.QueryResult) {
	for _, res := range resPVCostPerGiBHour {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := res.GetString("volumename")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: PV cost without volumename")
			continue
		}

		key := newPVKey(cluster, name)

		pvMap[key] = &PV{
			Cluster:        cluster,
			Name:           name,
			CostPerGiBHour: res.Values[0].Value,
		}
	}
}

func applyPVBytes(pvMap map[pvKey]*PV, resPVBytes []*prom.QueryResult) {
	for _, res := range resPVBytes {
		key, err := resultPVKey(res, env.GetPromClusterLabel(), "persistentvolume")
		if err != nil {
			log.Warnf("CostModel.ComputeAllocation: PV bytes query result missing field: %s", err)
			continue
		}

		if _, ok := pvMap[key]; !ok {
			log.Warnf("CostModel.ComputeAllocation: PV bytes result for missing PV: %s", err)
			continue
		}

		pvMap[key].Bytes = res.Values[0].Value
	}
}

func buildPVCMap(window kubecost.Window, pvcMap map[pvcKey]*PVC, pvMap map[pvKey]*PV, resPVCInfo []*prom.QueryResult) {
	for _, res := range resPVCInfo {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		values, err := res.GetStrings("persistentvolumeclaim", "storageclass", "volumename", "namespace")
		if err != nil {
			log.DedupedWarningf(10, "CostModel.ComputeAllocation: PVC info query result missing field: %s", err)
			continue
		}

		namespace := values["namespace"]
		name := values["persistentvolumeclaim"]
		volume := values["volumename"]
		storageClass := values["storageclass"]

		pvKey := newPVKey(cluster, volume)
		pvcKey := newPVCKey(cluster, namespace, name)

		// pvcStart and pvcEnd are the timestamps of the first and last minutes
		// the PVC was running, respectively. We subtract 1m from pvcStart
		// because this point will actually represent the end of the first
		// minute. We don't subtract from pvcEnd because it already represents
		// the end of the last minute.
		var pvcStart, pvcEnd time.Time
		for _, datum := range res.Values {
			t := time.Unix(int64(datum.Timestamp), 0)
			if pvcStart.IsZero() && datum.Value > 0 && window.Contains(t) {
				pvcStart = t
			}
			if datum.Value > 0 && window.Contains(t) {
				pvcEnd = t
			}
		}
		if pvcStart.IsZero() || pvcEnd.IsZero() {
			log.Warnf("CostModel.ComputeAllocation: PVC %s has no running time", pvcKey)
		}
		pvcStart = pvcStart.Add(-time.Minute)

		if _, ok := pvMap[pvKey]; !ok {
			continue
		}

		pvMap[pvKey].StorageClass = storageClass

		if _, ok := pvcMap[pvcKey]; !ok {
			pvcMap[pvcKey] = &PVC{}
		}

		pvcMap[pvcKey].Name = name
		pvcMap[pvcKey].Namespace = namespace
		pvcMap[pvcKey].Volume = pvMap[pvKey]
		pvcMap[pvcKey].Start = pvcStart
		pvcMap[pvcKey].End = pvcEnd
	}
}

func applyPVCBytesRequested(pvcMap map[pvcKey]*PVC, resPVCBytesRequested []*prom.QueryResult) {
	for _, res := range resPVCBytesRequested {
		key, err := resultPVCKey(res, env.GetPromClusterLabel(), "namespace", "persistentvolumeclaim")
		if err != nil {
			continue
		}

		if _, ok := pvcMap[key]; !ok {
			continue
		}

		pvcMap[key].Bytes = res.Values[0].Value
	}
}

func buildPodPVCMap(podPVCMap map[podKey][]*PVC, pvMap map[pvKey]*PV, pvcMap map[pvcKey]*PVC, podMap map[podKey]*Pod, resPodPVCAllocation []*prom.QueryResult, podUIDKeyMap map[podKey][]podKey, ingestPodUID bool) {
	for _, res := range resPodPVCAllocation {
		cluster, err := res.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		values, err := res.GetStrings("persistentvolume", "persistentvolumeclaim", "pod", "namespace")
		if err != nil {
			log.DedupedWarningf(5, "CostModel.ComputeAllocation: PVC allocation query result missing field: %s", err)
			continue
		}

		namespace := values["namespace"]
		pod := values["pod"]
		name := values["persistentvolumeclaim"]
		volume := values["persistentvolume"]

		key := newPodKey(cluster, namespace, pod)
		pvKey := newPVKey(cluster, volume)
		pvcKey := newPVCKey(cluster, namespace, name)

		var keys []podKey

		if ingestPodUID {
			if uidKeys, ok := podUIDKeyMap[key]; ok {

				keys = append(keys, uidKeys...)

			}
		} else {
			keys = []podKey{key}
		}

		for _, key := range keys {

			if _, ok := pvMap[pvKey]; !ok {
				log.DedupedWarningf(5, "CostModel.ComputeAllocation: PV missing for PVC allocation query result: %s", pvKey)
				continue
			}

			if _, ok := podPVCMap[key]; !ok {
				podPVCMap[key] = []*PVC{}
			}

			pvc, ok := pvcMap[pvcKey]
			if !ok {
				log.DedupedWarningf(5, "CostModel.ComputeAllocation: PVC missing for PVC allocation query: %s", pvcKey)
				continue
			}

			count := 1
			if pod, ok := podMap[key]; ok && len(pod.Allocations) > 0 {
				count = len(pod.Allocations)
			} else {
				log.DedupedWarningf(10, "CostModel.ComputeAllocation: PVC %s for missing pod %s", pvcKey, key)
			}

			pvc.Count = count
			pvc.Mounted = true

			podPVCMap[key] = append(podPVCMap[key], pvc)
		}
	}
}

func applyUnmountedPVs(window kubecost.Window, podMap map[podKey]*Pod, pvMap map[pvKey]*PV, pvcMap map[pvcKey]*PVC) {
	unmountedPVBytes := map[string]float64{}
	unmountedPVCost := map[string]float64{}

	for _, pv := range pvMap {
		mounted := false
		for _, pvc := range pvcMap {
			if pvc.Volume == nil {
				continue
			}
			if pvc.Volume == pv {
				mounted = true
				break
			}
		}

		if !mounted {
			gib := pv.Bytes / 1024 / 1024 / 1024
			hrs := window.Minutes() / 60.0 // TODO improve with PV hours, not window hours
			cost := pv.CostPerGiBHour * gib * hrs
			unmountedPVCost[pv.Cluster] += cost
			unmountedPVBytes[pv.Cluster] += pv.Bytes
		}
	}

	for cluster, amount := range unmountedPVCost {
		container := kubecost.UnmountedSuffix
		pod := kubecost.UnmountedSuffix
		namespace := kubecost.UnmountedSuffix
		node := ""

		key := newPodKey(cluster, namespace, pod)
		podMap[key] = &Pod{
			Window:      window.Clone(),
			Start:       *window.Start(),
			End:         *window.End(),
			Key:         key,
			Allocations: map[string]*kubecost.Allocation{},
		}

		podMap[key].AppendContainer(container)
		podMap[key].Allocations[container].Properties.Cluster = cluster
		podMap[key].Allocations[container].Properties.Node = node
		podMap[key].Allocations[container].Properties.Namespace = namespace
		podMap[key].Allocations[container].Properties.Pod = pod
		podMap[key].Allocations[container].Properties.Container = container
		pvKey := kubecost.PVKey{
			Cluster: cluster,
			Name:    kubecost.UnmountedSuffix,
		}
		unmountedPVs := kubecost.PVAllocations{
			pvKey: {
				ByteHours: unmountedPVBytes[cluster] * window.Minutes() / 60.0,
				Cost:      amount,
			},
		}
		podMap[key].Allocations[container].PVs = podMap[key].Allocations[container].PVs.Add(unmountedPVs)
	}
}

func applyUnmountedPVCs(window kubecost.Window, podMap map[podKey]*Pod, pvcMap map[pvcKey]*PVC) {
	unmountedPVCBytes := map[namespaceKey]float64{}
	unmountedPVCCost := map[namespaceKey]float64{}

	for _, pvc := range pvcMap {
		if !pvc.Mounted && pvc.Volume != nil {
			key := newNamespaceKey(pvc.Cluster, pvc.Namespace)

			gib := pvc.Volume.Bytes / 1024 / 1024 / 1024
			hrs := pvc.Minutes() / 60.0
			cost := pvc.Volume.CostPerGiBHour * gib * hrs
			unmountedPVCCost[key] += cost
			unmountedPVCBytes[key] += pvc.Volume.Bytes
		}
	}

	for key, amount := range unmountedPVCCost {
		container := kubecost.UnmountedSuffix
		pod := kubecost.UnmountedSuffix
		namespace := key.Namespace
		node := ""
		cluster := key.Cluster

		podKey := newPodKey(cluster, namespace, pod)
		podMap[podKey] = &Pod{
			Window:      window.Clone(),
			Start:       *window.Start(),
			End:         *window.End(),
			Key:         podKey,
			Allocations: map[string]*kubecost.Allocation{},
		}

		podMap[podKey].AppendContainer(container)
		podMap[podKey].Allocations[container].Properties.Cluster = cluster
		podMap[podKey].Allocations[container].Properties.Node = node
		podMap[podKey].Allocations[container].Properties.Namespace = namespace
		podMap[podKey].Allocations[container].Properties.Pod = pod
		podMap[podKey].Allocations[container].Properties.Container = container
		pvKey := kubecost.PVKey{
			Cluster: cluster,
			Name:    kubecost.UnmountedSuffix,
		}
		unmountedPVs := kubecost.PVAllocations{
			pvKey: {
				ByteHours: unmountedPVCBytes[key] * window.Minutes() / 60.0,
				Cost:      amount,
			},
		}
		podMap[podKey].Allocations[container].PVs = podMap[podKey].Allocations[container].PVs.Add(unmountedPVs)

	}
}

// LB describes the start and end time of a Load Balancer along with cost
type LB struct {
	TotalCost float64
	Start     time.Time
	End       time.Time
}

func getLoadBalancerCosts(resLBCost, resLBActiveMins []*prom.QueryResult, resolution time.Duration) map[serviceKey]*LB {
	lbMap := make(map[serviceKey]*LB)
	lbHourlyCosts := make(map[serviceKey]float64)
	for _, res := range resLBCost {
		serviceKey, err := resultServiceKey(res, env.GetPromClusterLabel(), "namespace", "service_name")
		if err != nil {
			continue
		}
		lbHourlyCosts[serviceKey] = res.Values[0].Value
	}
	for _, res := range resLBActiveMins {
		serviceKey, err := resultServiceKey(res, env.GetPromClusterLabel(), "namespace", "service_name")
		if err != nil || len(res.Values) == 0 {
			continue
		}
		if _, ok := lbHourlyCosts[serviceKey]; !ok {
			log.Warnf("CostModel: failed to find hourly cost for Load Balancer: %v", serviceKey)
			continue
		}

		s := time.Unix(int64(res.Values[0].Timestamp), 0)
		// subtract resolution from start time to cover full time period
		s = s.Add(-resolution)
		e := time.Unix(int64(res.Values[len(res.Values)-1].Timestamp), 0)
		hours := e.Sub(s).Hours()

		lbMap[serviceKey] = &LB{
			TotalCost: lbHourlyCosts[serviceKey] * hours,
			Start:     s,
			End:       e,
		}
	}
	return lbMap
}

func applyLoadBalancersToPods(lbMap map[serviceKey]*LB, allocsByService map[serviceKey][]*kubecost.Allocation) {
	for sKey, lb := range lbMap {
		totalHours := 0.0
		allocHours := make(map[*kubecost.Allocation]float64)
		// Add portion of load balancing cost to each allocation
		// proportional to the total number of hours allocations used the load balancer
		for _, alloc := range allocsByService[sKey] {
			// Determine the (start, end) of the relationship between the
			// given LB and the associated Allocation so that a precise
			// number of hours can be used to compute cumulative cost.
			s, e := alloc.Start, alloc.End
			if lb.Start.After(alloc.Start) {
				s = lb.Start
			}
			if lb.End.Before(alloc.End) {
				e = lb.End
			}
			hours := e.Sub(s).Hours()
			// A negative number of hours signifies no overlap between the windows
			if hours > 0 {
				totalHours += hours
				allocHours[alloc] = hours
			}
		}

		// Distribute cost of service once total hours is calculated
		for alloc, hours := range allocHours {
			alloc.LoadBalancerCost += lb.TotalCost * hours / totalHours
		}
	}
}

// getNodePricing determines node pricing, given a key and a mapping from keys
// to their NodePricing instances, as well as the custom pricing configuration
// inherent to the CostModel instance. If custom pricing is set, use that. If
// not, use the pricing defined by the given key. If that doesn't exist, fall
// back on custom pricing as a default.
func (cm *CostModel) getNodePricing(nodeMap map[nodeKey]*NodePricing, nodeKey nodeKey) *NodePricing {
	// Find the relevant NodePricing, if it exists. If not, substitute the
	// custom NodePricing as a default.
	node, ok := nodeMap[nodeKey]
	if !ok || node == nil {
		if nodeKey.Node != "" {
			log.DedupedWarningf(5, "CostModel: failed to find node for %s", nodeKey)
		}
		return cm.getCustomNodePricing(false)
	}

	// If custom pricing is enabled and can be retrieved, override detected
	// node pricing with the custom values.
	customPricingConfig, err := cm.Provider.GetConfig()
	if err != nil {
		log.Warnf("CostModel: failed to load custom pricing: %s", err)
	}
	if cloud.CustomPricesEnabled(cm.Provider) && customPricingConfig != nil {
		return cm.getCustomNodePricing(node.Preemptible)
	}

	node.Source = "prometheus"

	// If any of the values are NaN or zero, replace them with the custom
	// values as default.
	// TODO:CLEANUP can't we parse these custom prices once? why do we store
	// them as strings like this?

	if node.CostPerCPUHr == 0 || math.IsNaN(node.CostPerCPUHr) {
		log.Warnf("CostModel: node pricing has illegal CostPerCPUHr; replacing with custom pricing: %s", nodeKey)
		cpuCostStr := customPricingConfig.CPU
		if node.Preemptible {
			cpuCostStr = customPricingConfig.SpotCPU
		}
		costPerCPUHr, err := strconv.ParseFloat(cpuCostStr, 64)
		if err != nil {
			log.Warnf("CostModel: custom pricing has illegal CPU cost: %s", cpuCostStr)
		}
		node.CostPerCPUHr = costPerCPUHr
		node.Source += "/customCPU"
	}

	if math.IsNaN(node.CostPerGPUHr) {
		log.Warnf("CostModel: node pricing has illegal CostPerGPUHr; replacing with custom pricing: %s", nodeKey)
		gpuCostStr := customPricingConfig.GPU
		if node.Preemptible {
			gpuCostStr = customPricingConfig.SpotGPU
		}
		costPerGPUHr, err := strconv.ParseFloat(gpuCostStr, 64)
		if err != nil {
			log.Warnf("CostModel: custom pricing has illegal GPU cost: %s", gpuCostStr)
		}
		node.CostPerGPUHr = costPerGPUHr
		node.Source += "/customGPU"
	}

	if node.CostPerRAMGiBHr == 0 || math.IsNaN(node.CostPerRAMGiBHr) {
		log.Warnf("CostModel: node pricing has illegal CostPerRAMHr; replacing with custom pricing: %s", nodeKey)
		ramCostStr := customPricingConfig.RAM
		if node.Preemptible {
			ramCostStr = customPricingConfig.SpotRAM
		}
		costPerRAMHr, err := strconv.ParseFloat(ramCostStr, 64)
		if err != nil {
			log.Warnf("CostModel: custom pricing has illegal RAM cost: %s", ramCostStr)
		}
		node.CostPerRAMGiBHr = costPerRAMHr
		node.Source += "/customRAM"
	}

	return node
}

// getCustomNodePricing converts the CostModel's configured custom pricing
// values into a NodePricing instance.
func (cm *CostModel) getCustomNodePricing(spot bool) *NodePricing {
	customPricingConfig, err := cm.Provider.GetConfig()
	if err != nil {
		return nil
	}

	cpuCostStr := customPricingConfig.CPU
	gpuCostStr := customPricingConfig.GPU
	ramCostStr := customPricingConfig.RAM
	if spot {
		cpuCostStr = customPricingConfig.SpotCPU
		gpuCostStr = customPricingConfig.SpotGPU
		ramCostStr = customPricingConfig.SpotRAM
	}

	node := &NodePricing{Source: "custom"}

	costPerCPUHr, err := strconv.ParseFloat(cpuCostStr, 64)
	if err != nil {
		log.Warnf("CostModel: custom pricing has illegal CPU cost: %s", cpuCostStr)
	}
	node.CostPerCPUHr = costPerCPUHr

	costPerGPUHr, err := strconv.ParseFloat(gpuCostStr, 64)
	if err != nil {
		log.Warnf("CostModel: custom pricing has illegal GPU cost: %s", gpuCostStr)
	}
	node.CostPerGPUHr = costPerGPUHr

	costPerRAMHr, err := strconv.ParseFloat(ramCostStr, 64)
	if err != nil {
		log.Warnf("CostModel: custom pricing has illegal RAM cost: %s", ramCostStr)
	}
	node.CostPerRAMGiBHr = costPerRAMHr

	return node
}

// NodePricing describes the resource costs associated with a given node, as
// well as the source of the information (e.g. prometheus, custom)
type NodePricing struct {
	Name            string
	NodeType        string
	ProviderID      string
	Preemptible     bool
	CostPerCPUHr    float64
	CostPerRAMGiBHr float64
	CostPerGPUHr    float64
	Discount        float64
	Source          string
}

// Pod describes a running pod's start and end time within a Window and
// all the Allocations (i.e. containers) contained within it.
type Pod struct {
	Window      kubecost.Window
	Start       time.Time
	End         time.Time
	Key         podKey
	Allocations map[string]*kubecost.Allocation
}

// AppendContainer adds an entry for the given container name to the Pod.
func (p Pod) AppendContainer(container string) {
	name := fmt.Sprintf("%s/%s/%s/%s", p.Key.Cluster, p.Key.Namespace, p.Key.Pod, container)

	alloc := &kubecost.Allocation{
		Name:       name,
		Properties: &kubecost.AllocationProperties{},
		Window:     p.Window.Clone(),
		Start:      p.Start,
		End:        p.End,
	}
	alloc.Properties.Container = container
	alloc.Properties.Pod = p.Key.Pod
	alloc.Properties.Namespace = p.Key.Namespace
	alloc.Properties.Cluster = p.Key.Cluster

	p.Allocations[container] = alloc
}

// PVC describes a PersistentVolumeClaim
// TODO:CLEANUP move to pkg/kubecost?
// TODO:CLEANUP add PersistentVolumeClaims field to type Allocation?
type PVC struct {
	Bytes     float64   `json:"bytes"`
	Count     int       `json:"count"`
	Name      string    `json:"name"`
	Cluster   string    `json:"cluster"`
	Namespace string    `json:"namespace"`
	Volume    *PV       `json:"persistentVolume"`
	Mounted   bool      `json:"mounted"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

// Cost computes the cumulative cost of the PVC
func (pvc *PVC) Cost() float64 {
	if pvc == nil || pvc.Volume == nil {
		return 0.0
	}

	gib := pvc.Bytes / 1024 / 1024 / 1024
	hrs := pvc.Minutes() / 60.0

	return pvc.Volume.CostPerGiBHour * gib * hrs
}

// Minutes computes the number of minutes over which the PVC is defined
func (pvc *PVC) Minutes() float64 {
	if pvc == nil {
		return 0.0
	}

	return pvc.End.Sub(pvc.Start).Minutes()
}

// String returns a string representation of the PVC
func (pvc *PVC) String() string {
	if pvc == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s/%s{Bytes:%.2f, Cost:%.6f, Start,End:%s}", pvc.Cluster, pvc.Namespace, pvc.Name, pvc.Bytes, pvc.Cost(), kubecost.NewWindow(&pvc.Start, &pvc.End))
}

// PV describes a PersistentVolume
// TODO:CLEANUP move to pkg/kubecost?
type PV struct {
	Bytes          float64 `json:"bytes"`
	CostPerGiBHour float64 `json:"costPerGiBHour"`
	Cluster        string  `json:"cluster"`
	Name           string  `json:"name"`
	StorageClass   string  `json:"storageClass"`
}

// String returns a string representation of the PV
func (pv *PV) String() string {
	if pv == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s{Bytes:%.2f, Cost/GiB*Hr:%.6f, StorageClass:%s}", pv.Cluster, pv.Name, pv.Bytes, pv.CostPerGiBHour, pv.StorageClass)
}
