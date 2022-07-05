package costmodel

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kubecost/opencost/pkg/kubecost"
	"github.com/kubecost/opencost/pkg/util/timeutil"

	"github.com/kubecost/opencost/pkg/cloud"
	"github.com/kubecost/opencost/pkg/env"
	"github.com/kubecost/opencost/pkg/log"
	"github.com/kubecost/opencost/pkg/prom"

	prometheus "github.com/prometheus/client_golang/api"
)

const (
	queryClusterCores = `sum(
		avg(avg_over_time(kube_node_status_capacity_cpu_cores{cluster="udaan-k8s0"}[%s] %s)) by (node, %s) * avg(avg_over_time(node_cpu_hourly_cost{cluster="udaan-k8s0"}[%s] %s)) by (node, %s) * 730 +
		avg(avg_over_time(node_gpu_hourly_cost{cluster="udaan-k8s0"}[%s] %s)) by (node, %s) * 730
	  ) by (%s)`

	queryClusterRAM = `sum(
		avg(avg_over_time(kube_node_status_capacity_memory_bytes{cluster="udaan-k8s0"}[%s] %s)) by (node, %s) / 1024 / 1024 / 1024 * avg(avg_over_time(node_ram_hourly_cost{cluster="udaan-k8s0"}[%s] %s)) by (node, %s) * 730
	  ) by (%s)`

	queryStorage = `sum(
		avg(avg_over_time(pv_hourly_cost{cluster="udaan-k8s0"}[%s] %s)) by (persistentvolume, %s) * 730
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes{cluster="udaan-k8s0"}[%s] %s)) by (persistentvolume, %s) / 1024 / 1024 / 1024
	  ) by (%s) %s`

	queryTotal = `sum(avg(node_total_hourly_cost{cluster="udaan-k8s0"}) by (node, %s)) * 730 +
	  sum(
		avg(avg_over_time(pv_hourly_cost{cluster="udaan-k8s0"}[1h])) by (persistentvolume, %s) * 730
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes{cluster="udaan-k8s0"}[1h])) by (persistentvolume, %s) / 1024 / 1024 / 1024
	  ) by (%s) %s`

	queryNodes = `sum(avg(node_total_hourly_cost{cluster="udaan-k8s0"}) by (node, %s)) * 730 %s`
)

const maxLocalDiskSize = 200 // AWS limits root disks to 100 Gi, and occasional metric errors in filesystem size should not contribute to large costs.

// Costs represents cumulative and monthly cluster costs over a given duration. Costs
// are broken down by cores, memory, and storage.
type ClusterCosts struct {
	Start             *time.Time             `json:"startTime"`
	End               *time.Time             `json:"endTime"`
	CPUCumulative     float64                `json:"cpuCumulativeCost"`
	CPUMonthly        float64                `json:"cpuMonthlyCost"`
	CPUBreakdown      *ClusterCostsBreakdown `json:"cpuBreakdown"`
	GPUCumulative     float64                `json:"gpuCumulativeCost"`
	GPUMonthly        float64                `json:"gpuMonthlyCost"`
	RAMCumulative     float64                `json:"ramCumulativeCost"`
	RAMMonthly        float64                `json:"ramMonthlyCost"`
	RAMBreakdown      *ClusterCostsBreakdown `json:"ramBreakdown"`
	StorageCumulative float64                `json:"storageCumulativeCost"`
	StorageMonthly    float64                `json:"storageMonthlyCost"`
	StorageBreakdown  *ClusterCostsBreakdown `json:"storageBreakdown"`
	TotalCumulative   float64                `json:"totalCumulativeCost"`
	TotalMonthly      float64                `json:"totalMonthlyCost"`
	DataMinutes       float64
}

// ClusterCostsBreakdown provides percentage-based breakdown of a resource by
// categories: user for user-space (i.e. non-system) usage, system, and idle.
type ClusterCostsBreakdown struct {
	Idle   float64 `json:"idle"`
	Other  float64 `json:"other"`
	System float64 `json:"system"`
	User   float64 `json:"user"`
}

// NewClusterCostsFromCumulative takes cumulative cost data over a given time range, computes
// the associated monthly rate data, and returns the Costs.
func NewClusterCostsFromCumulative(cpu, gpu, ram, storage float64, window, offset time.Duration, dataHours float64) (*ClusterCosts, error) {
	start, end := timeutil.ParseTimeRange(window, offset)

	// If the number of hours is not given (i.e. is zero) compute one from the window and offset
	if dataHours == 0 {
		dataHours = end.Sub(start).Hours()
	}

	// Do not allow zero-length windows to prevent divide-by-zero issues
	if dataHours == 0 {
		return nil, fmt.Errorf("illegal time range: window %s, offset %s", window, offset)
	}

	cc := &ClusterCosts{
		Start:             &start,
		End:               &end,
		CPUCumulative:     cpu,
		GPUCumulative:     gpu,
		RAMCumulative:     ram,
		StorageCumulative: storage,
		TotalCumulative:   cpu + gpu + ram + storage,
		CPUMonthly:        cpu / dataHours * (timeutil.HoursPerMonth),
		GPUMonthly:        gpu / dataHours * (timeutil.HoursPerMonth),
		RAMMonthly:        ram / dataHours * (timeutil.HoursPerMonth),
		StorageMonthly:    storage / dataHours * (timeutil.HoursPerMonth),
	}
	cc.TotalMonthly = cc.CPUMonthly + cc.GPUMonthly + cc.RAMMonthly + cc.StorageMonthly

	return cc, nil
}

type Disk struct {
	Cluster    string
	Name       string
	ProviderID string
	Cost       float64
	Bytes      float64
	Local      bool
	Start      time.Time
	End        time.Time
	Minutes    float64
	Breakdown  *ClusterCostsBreakdown
}

type DiskIdentifier struct {
	Cluster string
	Name    string
}

func ClusterDisks(client prometheus.Client, provider cloud.Provider, start, end time.Time) (map[DiskIdentifier]*Disk, error) {
	// Query for the duration between start and end
	durStr := timeutil.DurationString(end.Sub(start))
	if durStr == "" {
		return nil, fmt.Errorf("illegal duration value for %s", kubecost.NewClosedWindow(start, end))
	}

	// Start from the time "end", querying backwards
	t := end

	// minsPerResolution determines accuracy and resource use for the following
	// queries. Smaller values (higher resolution) result in better accuracy,
	// but more expensive queries, and vice-a-versa.
	minsPerResolution := 1
	resolution := time.Duration(minsPerResolution) * time.Minute

	// hourlyToCumulative is a scaling factor that, when multiplied by an hourly
	// value, converts it to a cumulative value; i.e.
	// [$/hr] * [min/res]*[hr/min] = [$/res]
	hourlyToCumulative := float64(minsPerResolution) * (1.0 / 60.0)

	// TODO niko/assets how do we not hard-code this price?
	costPerGBHr := 0.04 / 730.0

	ctx := prom.NewNamedContext(client, prom.ClusterContextName)
	queryPVCost := fmt.Sprintf(`avg(avg_over_time(pv_hourly_cost{cluster="udaan-k8s0"}[%s])) by (%s, persistentvolume,provider_id)`, durStr, env.GetPromClusterLabel())
	queryPVSize := fmt.Sprintf(`avg(avg_over_time(kube_persistentvolume_capacity_bytes{cluster="udaan-k8s0"}[%s])) by (%s, persistentvolume)`, durStr, env.GetPromClusterLabel())
	queryActiveMins := fmt.Sprintf(`count(pv_hourly_cost{cluster="udaan-k8s0"}) by (%s, persistentvolume)[%s:%dm]`, env.GetPromClusterLabel(), durStr, minsPerResolution)

	queryLocalStorageCost := fmt.Sprintf(`sum_over_time(sum(container_fs_limit_bytes{device!="tmpfs", id="/", cluster="udaan-k8s0"}) by (instance, %s)[%s:%dm]) / 1024 / 1024 / 1024 * %f * %f`, env.GetPromClusterLabel(), durStr, minsPerResolution, hourlyToCumulative, costPerGBHr)
	queryLocalStorageUsedCost := fmt.Sprintf(`sum_over_time(sum(container_fs_usage_bytes{device!="tmpfs", id="/", cluster="udaan-k8s0"}) by (instance, %s)[%s:%dm]) / 1024 / 1024 / 1024 * %f * %f`, env.GetPromClusterLabel(), durStr, minsPerResolution, hourlyToCumulative, costPerGBHr)
	queryLocalStorageBytes := fmt.Sprintf(`avg_over_time(sum(container_fs_limit_bytes{device!="tmpfs", id="/", cluster="udaan-k8s0"}) by (instance, %s)[%s:%dm])`, env.GetPromClusterLabel(), durStr, minsPerResolution)
	queryLocalActiveMins := fmt.Sprintf(`count(node_total_hourly_cost{cluster="udaan-k8s0"}) by (%s, node)[%s:%dm]`, env.GetPromClusterLabel(), durStr, minsPerResolution)

	resChPVCost := ctx.QueryAtTime(queryPVCost, t)
	resChPVSize := ctx.QueryAtTime(queryPVSize, t)
	resChActiveMins := ctx.QueryAtTime(queryActiveMins, t)
	resChLocalStorageCost := ctx.QueryAtTime(queryLocalStorageCost, t)
	resChLocalStorageUsedCost := ctx.QueryAtTime(queryLocalStorageUsedCost, t)
	resChLocalStorageBytes := ctx.QueryAtTime(queryLocalStorageBytes, t)
	resChLocalActiveMins := ctx.QueryAtTime(queryLocalActiveMins, t)

	resPVCost, _ := resChPVCost.Await()
	resPVSize, _ := resChPVSize.Await()
	resActiveMins, _ := resChActiveMins.Await()
	resLocalStorageCost, _ := resChLocalStorageCost.Await()
	resLocalStorageUsedCost, _ := resChLocalStorageUsedCost.Await()
	resLocalStorageBytes, _ := resChLocalStorageBytes.Await()
	resLocalActiveMins, _ := resChLocalActiveMins.Await()
	if ctx.HasErrors() {
		return nil, ctx.ErrorCollection()
	}

	diskMap := map[DiskIdentifier]*Disk{}

	pvCosts(diskMap, resolution, resActiveMins, resPVSize, resPVCost, provider)

	for _, result := range resLocalStorageCost {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("instance")
		if err != nil {
			log.Warnf("ClusterDisks: local storage data missing instance")
			continue
		}

		cost := result.Values[0].Value
		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			diskMap[key] = &Disk{
				Cluster:   cluster,
				Name:      name,
				Breakdown: &ClusterCostsBreakdown{},
				Local:     true,
			}
		}
		diskMap[key].Cost += cost
	}

	for _, result := range resLocalStorageUsedCost {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("instance")
		if err != nil {
			log.Warnf("ClusterDisks: local storage usage data missing instance")
			continue
		}

		cost := result.Values[0].Value
		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			diskMap[key] = &Disk{
				Cluster:   cluster,
				Name:      name,
				Breakdown: &ClusterCostsBreakdown{},
				Local:     true,
			}
		}
		diskMap[key].Breakdown.System = cost / diskMap[key].Cost
	}

	for _, result := range resLocalStorageBytes {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("instance")
		if err != nil {
			log.Warnf("ClusterDisks: local storage data missing instance")
			continue
		}

		bytes := result.Values[0].Value
		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			diskMap[key] = &Disk{
				Cluster:   cluster,
				Name:      name,
				Breakdown: &ClusterCostsBreakdown{},
				Local:     true,
			}
		}
		diskMap[key].Bytes = bytes
		if bytes/1024/1024/1024 > maxLocalDiskSize {
			log.DedupedWarningf(5, "Deleting large root disk/localstorage disk from analysis")
			delete(diskMap, key)
		}
	}

	for _, result := range resLocalActiveMins {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("node")
		if err != nil {
			log.DedupedWarningf(5, "ClusterDisks: local active mins data missing instance")
			continue
		}

		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			log.DedupedWarningf(5, "ClusterDisks: local active mins for unidentified disk or disk deleted from analysis")
			continue
		}

		if len(result.Values) == 0 {
			continue
		}

		s := time.Unix(int64(result.Values[0].Timestamp), 0)
		e := time.Unix(int64(result.Values[len(result.Values)-1].Timestamp), 0)
		mins := e.Sub(s).Minutes()

		// TODO niko/assets if mins >= threshold, interpolate for missing data?

		diskMap[key].End = e
		diskMap[key].Start = s
		diskMap[key].Minutes = mins
	}

	for _, disk := range diskMap {
		// Apply all remaining RAM to Idle
		disk.Breakdown.Idle = 1.0 - (disk.Breakdown.System + disk.Breakdown.Other + disk.Breakdown.User)

		// Set provider Id to the name for reconciliation
		if disk.ProviderID == "" {
			disk.ProviderID = disk.Name
		}
	}

	return diskMap, nil
}

type Node struct {
	Cluster         string
	Name            string
	ProviderID      string
	NodeType        string
	CPUCost         float64
	CPUCores        float64
	GPUCost         float64
	GPUCount        float64
	RAMCost         float64
	RAMBytes        float64
	Discount        float64
	Preemptible     bool
	CPUBreakdown    *ClusterCostsBreakdown
	RAMBreakdown    *ClusterCostsBreakdown
	Start           time.Time
	End             time.Time
	Minutes         float64
	Labels          map[string]string
	CostPerCPUHr    float64
	CostPerRAMGiBHr float64
	CostPerGPUHr    float64
}

// GKE lies about the number of cores e2 nodes have. This table
// contains a mapping from node type -> actual CPU cores
// for those cases.
var partialCPUMap = map[string]float64{
	"e2-micro":  0.25,
	"e2-small":  0.5,
	"e2-medium": 1.0,
}

type NodeIdentifier struct {
	Cluster    string
	Name       string
	ProviderID string
}

type nodeIdentifierNoProviderID struct {
	Cluster string
	Name    string
}

func costTimesMinuteAndCount(activeDataMap map[NodeIdentifier]activeData, costMap map[NodeIdentifier]float64, resourceCountMap map[nodeIdentifierNoProviderID]float64) {
	for k, v := range activeDataMap {
		keyNon := nodeIdentifierNoProviderID{
			Cluster: k.Cluster,
			Name:    k.Name,
		}
		if cost, ok := costMap[k]; ok {
			minutes := v.minutes
			count := 1.0
			if c, ok := resourceCountMap[keyNon]; ok {
				count = c
			}
			costMap[k] = cost * (minutes / 60) * count
		}
	}
}

func costTimesMinute(activeDataMap map[NodeIdentifier]activeData, costMap map[NodeIdentifier]float64) {
	for k, v := range activeDataMap {
		if cost, ok := costMap[k]; ok {
			minutes := v.minutes
			costMap[k] = cost * (minutes / 60)
		}
	}
}

func ClusterNodes(cp cloud.Provider, client prometheus.Client, start, end time.Time) (map[NodeIdentifier]*Node, error) {
	// Query for the duration between start and end
	durStr := timeutil.DurationString(end.Sub(start))
	if durStr == "" {
		return nil, fmt.Errorf("illegal duration value for %s", kubecost.NewClosedWindow(start, end))
	}

	// Start from the time "end", querying backwards
	t := end

	// minsPerResolution determines accuracy and resource use for the following
	// queries. Smaller values (higher resolution) result in better accuracy,
	// but more expensive queries, and vice-a-versa.
	minsPerResolution := 1
	resolution := time.Duration(minsPerResolution) * time.Minute

	requiredCtx := prom.NewNamedContext(client, prom.ClusterContextName)
	optionalCtx := prom.NewNamedContext(client, prom.ClusterOptionalContextName)

	queryNodeCPUHourlyCost := fmt.Sprintf(`avg(avg_over_time(node_cpu_hourly_cost[%s])) by (%s, node, instance_type, provider_id)`, durStr, env.GetPromClusterLabel())
	queryNodeCPUCores := fmt.Sprintf(`avg(avg_over_time(kube_node_status_capacity_cpu_cores[%s])) by (%s, node)`, durStr, env.GetPromClusterLabel())
	queryNodeRAMHourlyCost := fmt.Sprintf(`avg(avg_over_time(node_ram_hourly_cost[%s])) by (%s, node, instance_type, provider_id) / 1024 / 1024 / 1024`, durStr, env.GetPromClusterLabel())
	queryNodeRAMBytes := fmt.Sprintf(`avg(avg_over_time(kube_node_status_capacity_memory_bytes[%s])) by (%s, node)`, durStr, env.GetPromClusterLabel())
	queryNodeGPUCount := fmt.Sprintf(`avg(avg_over_time(node_gpu_count[%s])) by (%s, node, provider_id)`, durStr, env.GetPromClusterLabel())
	queryNodeGPUHourlyCost := fmt.Sprintf(`avg(avg_over_time(node_gpu_hourly_cost[%s])) by (%s, node, instance_type, provider_id)`, durStr, env.GetPromClusterLabel())
	queryNodeCPUModeTotal := fmt.Sprintf(`sum(rate(node_cpu_seconds_total[%s:%dm])) by (kubernetes_node, %s, mode)`, durStr, minsPerResolution, env.GetPromClusterLabel())
	queryNodeRAMSystemPct := fmt.Sprintf(`sum(sum_over_time(container_memory_working_set_bytes{container_name!="POD",container_name!="",namespace="kube-system"}[%s:%dm])) by (instance, %s) / avg(label_replace(sum(sum_over_time(kube_node_status_capacity_memory_bytes[%s:%dm])) by (node, %s), "instance", "$1", "node", "(.*)")) by (instance, %s)`, durStr, minsPerResolution, env.GetPromClusterLabel(), durStr, minsPerResolution, env.GetPromClusterLabel(), env.GetPromClusterLabel())
	queryNodeRAMUserPct := fmt.Sprintf(`sum(sum_over_time(container_memory_working_set_bytes{container_name!="POD",container_name!="",namespace!="kube-system"}[%s:%dm])) by (instance, %s) / avg(label_replace(sum(sum_over_time(kube_node_status_capacity_memory_bytes[%s:%dm])) by (node, %s), "instance", "$1", "node", "(.*)")) by (instance, %s)`, durStr, minsPerResolution, env.GetPromClusterLabel(), durStr, minsPerResolution, env.GetPromClusterLabel(), env.GetPromClusterLabel())
	queryActiveMins := fmt.Sprintf(`avg(node_total_hourly_cost) by (node, %s, provider_id)[%s:%dm]`, env.GetPromClusterLabel(), durStr, minsPerResolution)
	queryIsSpot := fmt.Sprintf(`avg_over_time(kubecost_node_is_spot[%s:%dm])`, durStr, minsPerResolution)
	queryLabels := fmt.Sprintf(`count_over_time(kube_node_labels[%s:%dm])`, durStr, minsPerResolution)

	// Return errors if these fail
	resChNodeCPUHourlyCost := requiredCtx.QueryAtTime(queryNodeCPUHourlyCost, t)
	resChNodeCPUCores := requiredCtx.QueryAtTime(queryNodeCPUCores, t)
	resChNodeRAMHourlyCost := requiredCtx.QueryAtTime(queryNodeRAMHourlyCost, t)
	resChNodeRAMBytes := requiredCtx.QueryAtTime(queryNodeRAMBytes, t)
	resChNodeGPUCount := requiredCtx.QueryAtTime(queryNodeGPUCount, t)
	resChNodeGPUHourlyCost := requiredCtx.QueryAtTime(queryNodeGPUHourlyCost, t)
	resChActiveMins := requiredCtx.QueryAtTime(queryActiveMins, t)
	resChIsSpot := requiredCtx.QueryAtTime(queryIsSpot, t)

	// Do not return errors if these fail, but log warnings
	resChNodeCPUModeTotal := optionalCtx.QueryAtTime(queryNodeCPUModeTotal, t)
	resChNodeRAMSystemPct := optionalCtx.QueryAtTime(queryNodeRAMSystemPct, t)
	resChNodeRAMUserPct := optionalCtx.QueryAtTime(queryNodeRAMUserPct, t)
	resChLabels := optionalCtx.QueryAtTime(queryLabels, t)

	resNodeCPUHourlyCost, _ := resChNodeCPUHourlyCost.Await()
	resNodeCPUCores, _ := resChNodeCPUCores.Await()
	resNodeGPUCount, _ := resChNodeGPUCount.Await()
	resNodeGPUHourlyCost, _ := resChNodeGPUHourlyCost.Await()
	resNodeRAMHourlyCost, _ := resChNodeRAMHourlyCost.Await()
	resNodeRAMBytes, _ := resChNodeRAMBytes.Await()
	resIsSpot, _ := resChIsSpot.Await()
	resNodeCPUModeTotal, _ := resChNodeCPUModeTotal.Await()
	resNodeRAMSystemPct, _ := resChNodeRAMSystemPct.Await()
	resNodeRAMUserPct, _ := resChNodeRAMUserPct.Await()
	resActiveMins, _ := resChActiveMins.Await()
	resLabels, _ := resChLabels.Await()

	if optionalCtx.HasErrors() {
		for _, err := range optionalCtx.Errors() {
			log.Warnf("ClusterNodes: %s", err)
		}
	}
	if requiredCtx.HasErrors() {
		for _, err := range requiredCtx.Errors() {
			log.Errorf("ClusterNodes: %s", err)
		}

		return nil, requiredCtx.ErrorCollection()
	}

	activeDataMap := buildActiveDataMap(resActiveMins, resolution)

	gpuCountMap := buildGPUCountMap(resNodeGPUCount)
	preemptibleMap := buildPreemptibleMap(resIsSpot)

	cpuCostMap, clusterAndNameToType1 := buildCPUCostMap(resNodeCPUHourlyCost, cp, preemptibleMap)
	ramCostMap, clusterAndNameToType2 := buildRAMCostMap(resNodeRAMHourlyCost, cp, preemptibleMap)
	gpuCostMap, clusterAndNameToType3 := buildGPUCostMap(resNodeGPUHourlyCost, gpuCountMap, cp, preemptibleMap)

	clusterAndNameToTypeIntermediate := mergeTypeMaps(clusterAndNameToType1, clusterAndNameToType2)
	clusterAndNameToType := mergeTypeMaps(clusterAndNameToTypeIntermediate, clusterAndNameToType3)

	cpuCoresMap := buildCPUCoresMap(resNodeCPUCores)
	ramBytesMap := buildRAMBytesMap(resNodeRAMBytes)

	ramUserPctMap := buildRAMUserPctMap(resNodeRAMUserPct)
	ramSystemPctMap := buildRAMSystemPctMap(resNodeRAMSystemPct)

	cpuBreakdownMap := buildCPUBreakdownMap(resNodeCPUModeTotal)

	labelsMap := buildLabelsMap(resLabels)

	costTimesMinuteAndCount(activeDataMap, cpuCostMap, cpuCoresMap)
	costTimesMinuteAndCount(activeDataMap, ramCostMap, ramBytesMap)
	costTimesMinute(activeDataMap, gpuCostMap) // there's no need to do a weird "nodeIdentifierNoProviderID" type match since gpuCounts have a providerID

	nodeMap := buildNodeMap(
		cpuCostMap, ramCostMap, gpuCostMap, gpuCountMap,
		cpuCoresMap, ramBytesMap, ramUserPctMap,
		ramSystemPctMap,
		cpuBreakdownMap,
		activeDataMap,
		preemptibleMap,
		labelsMap,
		clusterAndNameToType,
		resolution,
	)

	c, err := cp.GetConfig()
	if err != nil {
		return nil, err
	}

	discount, err := ParsePercentString(c.Discount)
	if err != nil {
		return nil, err
	}

	negotiatedDiscount, err := ParsePercentString(c.NegotiatedDiscount)
	if err != nil {
		return nil, err
	}

	for _, node := range nodeMap {
		// TODO take GKE Reserved Instances into account
		node.Discount = cp.CombinedDiscountForNode(node.NodeType, node.Preemptible, discount, negotiatedDiscount)

		// Apply all remaining resources to Idle
		node.CPUBreakdown.Idle = 1.0 - (node.CPUBreakdown.System + node.CPUBreakdown.Other + node.CPUBreakdown.User)
		node.RAMBreakdown.Idle = 1.0 - (node.RAMBreakdown.System + node.RAMBreakdown.Other + node.RAMBreakdown.User)
	}

	return nodeMap, nil
}

type LoadBalancerIdentifier struct {
	Cluster   string
	Namespace string
	Name      string
}

type LoadBalancer struct {
	Cluster    string
	Namespace  string
	Name       string
	ProviderID string
	Cost       float64
	Start      time.Time
	End        time.Time
	Minutes    float64
}

func ClusterLoadBalancers(client prometheus.Client, start, end time.Time) (map[LoadBalancerIdentifier]*LoadBalancer, error) {
	// Query for the duration between start and end
	durStr := timeutil.DurationString(end.Sub(start))
	if durStr == "" {
		return nil, fmt.Errorf("illegal duration value for %s", kubecost.NewClosedWindow(start, end))
	}

	// Start from the time "end", querying backwards
	t := end

	// minsPerResolution determines accuracy and resource use for the following
	// queries. Smaller values (higher resolution) result in better accuracy,
	// but more expensive queries, and vice-a-versa.
	minsPerResolution := 1

	ctx := prom.NewNamedContext(client, prom.ClusterContextName)

	queryLBCost := fmt.Sprintf(`avg(avg_over_time(kubecost_load_balancer_cost{cluster="udaan-k8s0"}[%s])) by (namespace, service_name, %s, ingress_ip)`, durStr, env.GetPromClusterLabel())
	queryActiveMins := fmt.Sprintf(`avg(kubecost_load_balancer_cost{cluster="udaan-k8s0"}) by (namespace, service_name, %s, ingress_ip)[%s:%dm]`, env.GetPromClusterLabel(), durStr, minsPerResolution)

	resChLBCost := ctx.QueryAtTime(queryLBCost, t)
	resChActiveMins := ctx.QueryAtTime(queryActiveMins, t)

	resLBCost, _ := resChLBCost.Await()
	resActiveMins, _ := resChActiveMins.Await()

	if ctx.HasErrors() {
		return nil, ctx.ErrorCollection()
	}

	loadBalancerMap := make(map[LoadBalancerIdentifier]*LoadBalancer, len(resActiveMins))

	for _, result := range resActiveMins {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}
		namespace, err := result.GetString("namespace")
		if err != nil {
			log.Warnf("ClusterLoadBalancers: LB cost data missing namespace")
			continue
		}
		name, err := result.GetString("service_name")
		if err != nil {
			log.Warnf("ClusterLoadBalancers: LB cost data missing service_name")
			continue
		}
		providerID, err := result.GetString("ingress_ip")
		if err != nil {
			log.DedupedWarningf(5, "ClusterLoadBalancers: LB cost data missing ingress_ip")
			providerID = ""
		}

		key := LoadBalancerIdentifier{
			Cluster:   cluster,
			Namespace: namespace,
			Name:      name,
		}

		// Skip if there are no data
		if len(result.Values) == 0 {
			continue
		}

		// Add load balancer to the set of load balancers
		if _, ok := loadBalancerMap[key]; !ok {
			loadBalancerMap[key] = &LoadBalancer{
				Cluster:    cluster,
				Namespace:  namespace,
				Name:       fmt.Sprintf("%s/%s", namespace, name), // TODO:ETL this is kept for backwards-compatibility, but not good
				ProviderID: cloud.ParseLBID(providerID),
			}
		}

		// Append start, end, and minutes. This should come before all other data.
		s := time.Unix(int64(result.Values[0].Timestamp), 0)
		e := time.Unix(int64(result.Values[len(result.Values)-1].Timestamp), 0)
		loadBalancerMap[key].Start = s
		loadBalancerMap[key].End = e
		loadBalancerMap[key].Minutes = e.Sub(s).Minutes()

		// Fill in Provider ID if it is available and missing in the loadBalancerMap
		// Prevents there from being a duplicate LoadBalancers on the same day
		if providerID != "" && loadBalancerMap[key].ProviderID == "" {
			loadBalancerMap[key].ProviderID = providerID
		}
	}

	for _, result := range resLBCost {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}
		namespace, err := result.GetString("namespace")
		if err != nil {
			log.Warnf("ClusterLoadBalancers: LB cost data missing namespace")
			continue
		}
		name, err := result.GetString("service_name")
		if err != nil {
			log.Warnf("ClusterLoadBalancers: LB cost data missing service_name")
			continue
		}

		key := LoadBalancerIdentifier{
			Cluster:   cluster,
			Namespace: namespace,
			Name:      name,
		}

		// Apply cost as price-per-hour * hours
		if lb, ok := loadBalancerMap[key]; ok {
			lbPricePerHr := result.Values[0].Value
			hrs := lb.Minutes / 60.0
			lb.Cost += lbPricePerHr * hrs
		} else {
			log.DedupedWarningf(20, "ClusterLoadBalancers: found minutes for key that does not exist: %s", key)
		}
	}

	return loadBalancerMap, nil
}

// ComputeClusterCosts gives the cumulative and monthly-rate cluster costs over a window of time for all clusters.
func (a *Accesses) ComputeClusterCosts(client prometheus.Client, provider cloud.Provider, window, offset time.Duration, withBreakdown bool) (map[string]*ClusterCosts, error) {
	if window < 10*time.Minute {
		return nil, fmt.Errorf("minimum window of 10m required; got %s", window)
	}

	// Compute number of minutes in the full interval, for use interpolating missed scrapes or scaling missing data
	start, end := timeutil.ParseTimeRange(window, offset)

	mins := end.Sub(start).Minutes()

	windowStr := timeutil.DurationString(window)

	// minsPerResolution determines accuracy and resource use for the following
	// queries. Smaller values (higher resolution) result in better accuracy,
	// but more expensive queries, and vice-a-versa.
	minsPerResolution := 5

	// hourlyToCumulative is a scaling factor that, when multiplied by an hourly
	// value, converts it to a cumulative value; i.e.
	// [$/hr] * [min/res]*[hr/min] = [$/res]
	hourlyToCumulative := float64(minsPerResolution) * (1.0 / 60.0)

	const fmtQueryDataCount = `
		count_over_time(sum(kube_node_status_capacity_cpu_cores{cluster="udaan-k8s0"}) by (%s)[%s:%dm]%s) * %d
	`

	const fmtQueryTotalGPU = `
		sum(
			sum_over_time(node_gpu_hourly_cost{cluster="udaan-k8s0"}[%s:%dm]%s) * %f
		) by (%s)
	`

	const fmtQueryTotalCPU = `
		sum(
			sum_over_time(avg(kube_node_status_capacity_cpu_cores{cluster="udaan-k8s0"}) by (node, %s)[%s:%dm]%s) *
			avg(avg_over_time(node_cpu_hourly_cost{cluster="udaan-k8s0"}[%s:%dm]%s)) by (node, %s) * %f
		) by (%s)
	`

	const fmtQueryTotalRAM = `
		sum(
			sum_over_time(avg(kube_node_status_capacity_memory_bytes{cluster="udaan-k8s0"}) by (node, %s)[%s:%dm]%s) / 1024 / 1024 / 1024 *
			avg(avg_over_time(node_ram_hourly_cost{cluster="udaan-k8s0"}[%s:%dm]%s)) by (node, %s) * %f
		) by (%s)
	`

	const fmtQueryTotalStorage = `
		sum(
			sum_over_time(avg(kube_persistentvolume_capacity_bytes{cluster="udaan-k8s0"}) by (persistentvolume, %s)[%s:%dm]%s) / 1024 / 1024 / 1024 *
			avg(avg_over_time(pv_hourly_cost{cluster="udaan-k8s0"}[%s:%dm]%s)) by (persistentvolume, %s) * %f
		) by (%s)
	`

	const fmtQueryCPUModePct = `
		sum(rate(node_cpu_seconds_total{cluster="udaan-k8s0"}[%s]%s)) by (%s, mode) / ignoring(mode)
		group_left sum(rate(node_cpu_seconds_total{cluster="udaan-k8s0"}[%s]%s)) by (%s)
	`

	const fmtQueryRAMSystemPct = `
		sum(sum_over_time(container_memory_usage_bytes{container_name!="",namespace="kube-system", cluster="udaan-k8s0"}[%s:%dm]%s)) by (%s)
		/ sum(sum_over_time(kube_node_status_capacity_memory_bytes{cluster="udaan-k8s0"}[%s:%dm]%s)) by (%s)
	`

	const fmtQueryRAMUserPct = `
		sum(sum_over_time(kubecost_cluster_memory_working_set_bytes{cluster="udaan-k8s0"}[%s:%dm]%s)) by (%s)
		/ sum(sum_over_time(kube_node_status_capacity_memory_bytes{cluster="udaan-k8s0"}[%s:%dm]%s)) by (%s)
	`

	// TODO niko/clustercost metric "kubelet_volume_stats_used_bytes" was deprecated in 1.12, then seems to have come back in 1.17
	// const fmtQueryPVStorageUsePct = `(sum(kube_persistentvolumeclaim_info) by (persistentvolumeclaim, storageclass,namespace) + on (persistentvolumeclaim,namespace)
	// group_right(storageclass) sum(kubelet_volume_stats_used_bytes) by (persistentvolumeclaim,namespace))`

	queryUsedLocalStorage := provider.GetLocalStorageQuery(window, offset, false, true)

	queryTotalLocalStorage := provider.GetLocalStorageQuery(window, offset, false, false)
	if queryTotalLocalStorage != "" {
		queryTotalLocalStorage = fmt.Sprintf(" + %s", queryTotalLocalStorage)
	}

	fmtOffset := timeutil.DurationToPromOffsetString(offset)

	queryDataCount := fmt.Sprintf(fmtQueryDataCount, env.GetPromClusterLabel(), windowStr, minsPerResolution, fmtOffset, minsPerResolution)
	queryTotalGPU := fmt.Sprintf(fmtQueryTotalGPU, windowStr, minsPerResolution, fmtOffset, hourlyToCumulative, env.GetPromClusterLabel())
	queryTotalCPU := fmt.Sprintf(fmtQueryTotalCPU, env.GetPromClusterLabel(), windowStr, minsPerResolution, fmtOffset, windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel(), hourlyToCumulative, env.GetPromClusterLabel())
	queryTotalRAM := fmt.Sprintf(fmtQueryTotalRAM, env.GetPromClusterLabel(), windowStr, minsPerResolution, fmtOffset, windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel(), hourlyToCumulative, env.GetPromClusterLabel())
	queryTotalStorage := fmt.Sprintf(fmtQueryTotalStorage, env.GetPromClusterLabel(), windowStr, minsPerResolution, fmtOffset, windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel(), hourlyToCumulative, env.GetPromClusterLabel())

	ctx := prom.NewNamedContext(client, prom.ClusterContextName)

	resChs := ctx.QueryAll(
		queryDataCount,
		queryTotalGPU,
		queryTotalCPU,
		queryTotalRAM,
		queryTotalStorage,
	)

	// Only submit the local storage query if it is valid. Otherwise Prometheus
	// will return errors. Always append something to resChs, regardless, to
	// maintain indexing.
	if queryTotalLocalStorage != "" {
		resChs = append(resChs, ctx.Query(queryTotalLocalStorage))
	} else {
		resChs = append(resChs, nil)
	}

	if withBreakdown {
		queryCPUModePct := fmt.Sprintf(fmtQueryCPUModePct, windowStr, fmtOffset, env.GetPromClusterLabel(), windowStr, fmtOffset, env.GetPromClusterLabel())
		queryRAMSystemPct := fmt.Sprintf(fmtQueryRAMSystemPct, windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel(), windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel())
		queryRAMUserPct := fmt.Sprintf(fmtQueryRAMUserPct, windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel(), windowStr, minsPerResolution, fmtOffset, env.GetPromClusterLabel())

		bdResChs := ctx.QueryAll(
			queryCPUModePct,
			queryRAMSystemPct,
			queryRAMUserPct,
		)

		// Only submit the local storage query if it is valid. Otherwise Prometheus
		// will return errors. Always append something to resChs, regardless, to
		// maintain indexing.
		if queryUsedLocalStorage != "" {
			bdResChs = append(bdResChs, ctx.Query(queryUsedLocalStorage))
		} else {
			bdResChs = append(bdResChs, nil)
		}

		resChs = append(resChs, bdResChs...)
	}

	resDataCount, _ := resChs[0].Await()
	resTotalGPU, _ := resChs[1].Await()
	resTotalCPU, _ := resChs[2].Await()
	resTotalRAM, _ := resChs[3].Await()
	resTotalStorage, _ := resChs[4].Await()
	if ctx.HasErrors() {
		return nil, ctx.ErrorCollection()
	}

	defaultClusterID := env.GetClusterID()

	dataMinsByCluster := map[string]float64{}
	for _, result := range resDataCount {
		clusterID, _ := result.GetString(env.GetPromClusterLabel())
		if clusterID == "" {
			clusterID = defaultClusterID
		}
		dataMins := mins
		if len(result.Values) > 0 {
			dataMins = result.Values[0].Value
		} else {
			log.Warnf("Cluster cost data count returned no results for cluster %s", clusterID)
		}
		dataMinsByCluster[clusterID] = dataMins
	}

	// Determine combined discount
	discount, customDiscount := 0.0, 0.0
	c, err := a.CloudProvider.GetConfig()
	if err == nil {
		discount, err = ParsePercentString(c.Discount)
		if err != nil {
			discount = 0.0
		}
		customDiscount, err = ParsePercentString(c.NegotiatedDiscount)
		if err != nil {
			customDiscount = 0.0
		}
	}

	// Intermediate structure storing mapping of [clusterID][type ∈ {cpu, ram, storage, total}]=cost
	costData := make(map[string]map[string]float64)

	// Helper function to iterate over Prom query results, parsing the raw values into
	// the intermediate costData structure.
	setCostsFromResults := func(costData map[string]map[string]float64, results []*prom.QueryResult, name string, discount float64, customDiscount float64) {
		for _, result := range results {
			clusterID, _ := result.GetString(env.GetPromClusterLabel())
			if clusterID == "" {
				clusterID = defaultClusterID
			}
			if _, ok := costData[clusterID]; !ok {
				costData[clusterID] = map[string]float64{}
			}
			if len(result.Values) > 0 {
				costData[clusterID][name] += result.Values[0].Value * (1.0 - discount) * (1.0 - customDiscount)
				costData[clusterID]["total"] += result.Values[0].Value * (1.0 - discount) * (1.0 - customDiscount)
			}
		}
	}
	// Apply both sustained use and custom discounts to RAM and CPU
	setCostsFromResults(costData, resTotalCPU, "cpu", discount, customDiscount)
	setCostsFromResults(costData, resTotalRAM, "ram", discount, customDiscount)
	// Apply only custom discount to GPU and storage
	setCostsFromResults(costData, resTotalGPU, "gpu", 0.0, customDiscount)
	setCostsFromResults(costData, resTotalStorage, "storage", 0.0, customDiscount)
	if queryTotalLocalStorage != "" {
		resTotalLocalStorage, err := resChs[5].Await()
		if err != nil {
			return nil, err
		}
		setCostsFromResults(costData, resTotalLocalStorage, "localstorage", 0.0, customDiscount)
	}

	cpuBreakdownMap := map[string]*ClusterCostsBreakdown{}
	ramBreakdownMap := map[string]*ClusterCostsBreakdown{}
	pvUsedCostMap := map[string]float64{}
	if withBreakdown {
		resCPUModePct, _ := resChs[6].Await()
		resRAMSystemPct, _ := resChs[7].Await()
		resRAMUserPct, _ := resChs[8].Await()
		if ctx.HasErrors() {
			return nil, ctx.ErrorCollection()
		}

		for _, result := range resCPUModePct {
			clusterID, _ := result.GetString(env.GetPromClusterLabel())
			if clusterID == "" {
				clusterID = defaultClusterID
			}
			if _, ok := cpuBreakdownMap[clusterID]; !ok {
				cpuBreakdownMap[clusterID] = &ClusterCostsBreakdown{}
			}
			cpuBD := cpuBreakdownMap[clusterID]

			mode, err := result.GetString("mode")
			if err != nil {
				log.Warnf("ComputeClusterCosts: unable to read CPU mode: %s", err)
				mode = "other"
			}

			switch mode {
			case "idle":
				cpuBD.Idle += result.Values[0].Value
			case "system":
				cpuBD.System += result.Values[0].Value
			case "user":
				cpuBD.User += result.Values[0].Value
			default:
				cpuBD.Other += result.Values[0].Value
			}
		}

		for _, result := range resRAMSystemPct {
			clusterID, _ := result.GetString(env.GetPromClusterLabel())
			if clusterID == "" {
				clusterID = defaultClusterID
			}
			if _, ok := ramBreakdownMap[clusterID]; !ok {
				ramBreakdownMap[clusterID] = &ClusterCostsBreakdown{}
			}
			ramBD := ramBreakdownMap[clusterID]
			ramBD.System += result.Values[0].Value
		}
		for _, result := range resRAMUserPct {
			clusterID, _ := result.GetString(env.GetPromClusterLabel())
			if clusterID == "" {
				clusterID = defaultClusterID
			}
			if _, ok := ramBreakdownMap[clusterID]; !ok {
				ramBreakdownMap[clusterID] = &ClusterCostsBreakdown{}
			}
			ramBD := ramBreakdownMap[clusterID]
			ramBD.User += result.Values[0].Value
		}
		for _, ramBD := range ramBreakdownMap {
			remaining := 1.0
			remaining -= ramBD.Other
			remaining -= ramBD.System
			remaining -= ramBD.User
			ramBD.Idle = remaining
		}

		if queryUsedLocalStorage != "" {
			resUsedLocalStorage, err := resChs[9].Await()
			if err != nil {
				return nil, err
			}
			for _, result := range resUsedLocalStorage {
				clusterID, _ := result.GetString(env.GetPromClusterLabel())
				if clusterID == "" {
					clusterID = defaultClusterID
				}
				pvUsedCostMap[clusterID] += result.Values[0].Value
			}
		}
	}

	if ctx.HasErrors() {
		for _, err := range ctx.Errors() {
			log.Errorf("ComputeClusterCosts: %s", err)
		}
		return nil, ctx.ErrorCollection()
	}

	// Convert intermediate structure to Costs instances
	costsByCluster := map[string]*ClusterCosts{}
	for id, cd := range costData {
		dataMins, ok := dataMinsByCluster[id]
		if !ok {
			dataMins = mins
			log.Warnf("Cluster cost data count not found for cluster %s", id)
		}
		costs, err := NewClusterCostsFromCumulative(cd["cpu"], cd["gpu"], cd["ram"], cd["storage"]+cd["localstorage"], window, offset, dataMins/timeutil.MinsPerHour)
		if err != nil {
			log.Warnf("Failed to parse cluster costs on %s (%s) from cumulative data: %+v", window, offset, cd)
			return nil, err
		}

		if cpuBD, ok := cpuBreakdownMap[id]; ok {
			costs.CPUBreakdown = cpuBD
		}
		if ramBD, ok := ramBreakdownMap[id]; ok {
			costs.RAMBreakdown = ramBD
		}
		costs.StorageBreakdown = &ClusterCostsBreakdown{}
		if pvUC, ok := pvUsedCostMap[id]; ok {
			costs.StorageBreakdown.Idle = (costs.StorageCumulative - pvUC) / costs.StorageCumulative
			costs.StorageBreakdown.User = pvUC / costs.StorageCumulative
		}
		costs.DataMinutes = dataMins
		costsByCluster[id] = costs
	}

	return costsByCluster, nil
}

type Totals struct {
	TotalCost   [][]string `json:"totalcost"`
	CPUCost     [][]string `json:"cpucost"`
	MemCost     [][]string `json:"memcost"`
	StorageCost [][]string `json:"storageCost"`
}

func resultToTotals(qrs []*prom.QueryResult) ([][]string, error) {
	if len(qrs) == 0 {
		return [][]string{}, fmt.Errorf("Not enough data available in the selected time range")
	}

	result := qrs[0]
	totals := [][]string{}
	for _, value := range result.Values {
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		totals = append(totals, toAppend)
	}
	return totals, nil
}

// ClusterCostsOverTime gives the full cluster costs over time
func ClusterCostsOverTime(cli prometheus.Client, provider cloud.Provider, startString, endString string, window, offset time.Duration) (*Totals, error) {
	localStorageQuery := provider.GetLocalStorageQuery(window, offset, true, false)
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	layout := "2006-01-02T15:04:05.000Z"

	start, err := time.Parse(layout, startString)
	if err != nil {
		log.Errorf("Error parsing time %s. Error: %s", startString, err.Error())
		return nil, err
	}
	end, err := time.Parse(layout, endString)
	if err != nil {
		log.Errorf("Error parsing time %s. Error: %s", endString, err.Error())
		return nil, err
	}
	fmtWindow := timeutil.DurationString(window)

	if fmtWindow == "" {
		err := fmt.Errorf("window value invalid or missing")
		log.Errorf("Error parsing time %v. Error: %s", window, err.Error())
		return nil, err
	}

	fmtOffset := timeutil.DurationToPromOffsetString(offset)

	qCores := fmt.Sprintf(queryClusterCores, fmtWindow, fmtOffset, env.GetPromClusterLabel(), fmtWindow, fmtOffset, env.GetPromClusterLabel(), fmtWindow, fmtOffset, env.GetPromClusterLabel(), env.GetPromClusterLabel())
	qRAM := fmt.Sprintf(queryClusterRAM, fmtWindow, fmtOffset, env.GetPromClusterLabel(), fmtWindow, fmtOffset, env.GetPromClusterLabel(), env.GetPromClusterLabel())
	qStorage := fmt.Sprintf(queryStorage, fmtWindow, fmtOffset, env.GetPromClusterLabel(), fmtWindow, fmtOffset, env.GetPromClusterLabel(), env.GetPromClusterLabel(), localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, env.GetPromClusterLabel(), env.GetPromClusterLabel(), env.GetPromClusterLabel(), env.GetPromClusterLabel(), localStorageQuery)

	ctx := prom.NewNamedContext(cli, prom.ClusterContextName)
	resChClusterCores := ctx.QueryRange(qCores, start, end, window)
	resChClusterRAM := ctx.QueryRange(qRAM, start, end, window)
	resChStorage := ctx.QueryRange(qStorage, start, end, window)
	resChTotal := ctx.QueryRange(qTotal, start, end, window)

	resultClusterCores, err := resChClusterCores.Await()
	if err != nil {
		return nil, err
	}

	resultClusterRAM, err := resChClusterRAM.Await()
	if err != nil {
		return nil, err
	}

	resultStorage, err := resChStorage.Await()
	if err != nil {
		return nil, err
	}

	resultTotal, err := resChTotal.Await()
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotals(resultClusterCores)
	if err != nil {
		log.Infof("[Warning] ClusterCostsOverTime: no cpu data: %s", err)
		return nil, err
	}

	ramTotal, err := resultToTotals(resultClusterRAM)
	if err != nil {
		log.Infof("[Warning] ClusterCostsOverTime: no ram data: %s", err)
		return nil, err
	}

	storageTotal, err := resultToTotals(resultStorage)
	if err != nil {
		log.Infof("[Warning] ClusterCostsOverTime: no storage data: %s", err)
	}

	clusterTotal, err := resultToTotals(resultTotal)
	if err != nil {
		// If clusterTotal query failed, it's likely because there are no PVs, which
		// causes the qTotal query to return no data. Instead, query only node costs.
		// If that fails, return an error because something is actually wrong.
		qNodes := fmt.Sprintf(queryNodes, env.GetPromClusterLabel(), localStorageQuery)

		resultNodes, warnings, err := ctx.QueryRangeSync(qNodes, start, end, window)
		for _, warning := range warnings {
			log.Warnf(warning)
		}
		if err != nil {
			return nil, err
		}

		clusterTotal, err = resultToTotals(resultNodes)
		if err != nil {
			log.Infof("[Warning] ClusterCostsOverTime: no node data: %s", err)
			return nil, err
		}
	}

	return &Totals{
		TotalCost:   clusterTotal,
		CPUCost:     coreTotal,
		MemCost:     ramTotal,
		StorageCost: storageTotal,
	}, nil
}

func pvCosts(diskMap map[DiskIdentifier]*Disk, resolution time.Duration, resActiveMins, resPVSize, resPVCost []*prom.QueryResult, cp cloud.Provider) {
	for _, result := range resActiveMins {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("persistentvolume")
		if err != nil {
			log.Warnf("ClusterDisks: active mins missing pv name")
			continue
		}

		if len(result.Values) == 0 {
			continue
		}

		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			diskMap[key] = &Disk{
				Cluster:   cluster,
				Name:      name,
				Breakdown: &ClusterCostsBreakdown{},
			}
		}
		s := time.Unix(int64(result.Values[0].Timestamp), 0)
		e := time.Unix(int64(result.Values[len(result.Values)-1].Timestamp), 0)
		mins := e.Sub(s).Minutes()

		// TODO niko/assets if mins >= threshold, interpolate for missing data?

		diskMap[key].End = e
		diskMap[key].Start = s
		diskMap[key].Minutes = mins
	}

	for _, result := range resPVSize {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("persistentvolume")
		if err != nil {
			log.Warnf("ClusterDisks: PV size data missing persistentvolume")
			continue
		}

		// TODO niko/assets storage class

		bytes := result.Values[0].Value
		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			diskMap[key] = &Disk{
				Cluster:   cluster,
				Name:      name,
				Breakdown: &ClusterCostsBreakdown{},
			}
		}
		diskMap[key].Bytes = bytes
	}

	customPricingEnabled := cloud.CustomPricesEnabled(cp)
	customPricingConfig, err := cp.GetConfig()
	if err != nil {
		log.Warnf("ClusterDisks: failed to load custom pricing: %s", err)
	}

	for _, result := range resPVCost {
		cluster, err := result.GetString(env.GetPromClusterLabel())
		if err != nil {
			cluster = env.GetClusterID()
		}

		name, err := result.GetString("persistentvolume")
		if err != nil {
			log.Warnf("ClusterDisks: PV cost data missing persistentvolume")
			continue
		}

		// TODO niko/assets storage class

		var cost float64
		if customPricingEnabled && customPricingConfig != nil {
			customPVCostStr := customPricingConfig.Storage

			customPVCost, err := strconv.ParseFloat(customPVCostStr, 64)
			if err != nil {
				log.Warnf("ClusterDisks: error parsing custom PV price: %s", customPVCostStr)
			}

			cost = customPVCost
		} else {
			cost = result.Values[0].Value
		}

		key := DiskIdentifier{cluster, name}
		if _, ok := diskMap[key]; !ok {
			diskMap[key] = &Disk{
				Cluster:   cluster,
				Name:      name,
				Breakdown: &ClusterCostsBreakdown{},
			}
		}
		diskMap[key].Cost = cost * (diskMap[key].Bytes / 1024 / 1024 / 1024) * (diskMap[key].Minutes / 60)
		providerID, _ := result.GetString("provider_id") // just put the providerID set up here, it's the simplest query.
		if providerID != "" {
			diskMap[key].ProviderID = cloud.ParsePVID(providerID)
		}
	}
}
