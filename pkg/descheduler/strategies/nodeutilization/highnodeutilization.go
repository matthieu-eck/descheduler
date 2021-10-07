/*
Copyright 2021 The Kubernetes Authors.

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

package nodeutilization

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/descheduler/pkg/api"
	"sigs.k8s.io/descheduler/pkg/descheduler/evictions"
	nodeutil "sigs.k8s.io/descheduler/pkg/descheduler/node"
	"sigs.k8s.io/descheduler/pkg/utils"
)

// HighNodeUtilization evicts pods from under utilized nodes so that scheduler can schedule according to its strategy.
// Note that CPU/Memory requests are used to calculate nodes' utilization and not the actual resource usage.
func HighNodeUtilization(ctx context.Context, client clientset.Interface, strategy api.DeschedulerStrategy, nodes []*v1.Node, podEvictor *evictions.PodEvictor) {
	if err := validateNodeUtilizationParams(strategy.Params); err != nil {
		klog.ErrorS(err, "Invalid HighNodeUtilization parameters")
		return
	}

	nodeFit := false
	if strategy.Params != nil {
		nodeFit = strategy.Params.NodeFit
	}

	thresholdPriority, err := utils.GetPriorityFromStrategyParams(ctx, client, strategy.Params)
	if err != nil {
		klog.ErrorS(err, "Failed to get threshold priority from strategy's params")
		return
	}

	strategyConfig := strategy.Params.NodeResourceUtilizationThresholds
	strategyConfig.TargetThresholds = make(api.ResourceThresholds)

	if err := validateHighUtilizationStrategyConfig(strategyConfig); err != nil {
		klog.ErrorS(err, "HighNodeUtilization config is not valid")
		return
	}

	setDefaultForThresholds(strategyConfig)
	resourceNames := getResourceNames(strategyConfig.TargetThresholds)
	sourceNodes, highNodes := classifyNodes(
		getNodeUsage(ctx, client, nodes, strategyConfig, resourceNames),
		func(node *v1.Node, usage *NodeUsage) bool {
			return isNodeWithLowUtilization(usage)
		},
		func(node *v1.Node, usage *NodeUsage) bool {
			if nodeutil.IsNodeUnschedulable(node) {
				klog.V(2).InfoS("Node is unschedulable", "node", klog.KObj(node))
				return false
			}
			return !isNodeWithLowUtilization(usage)
		})

	// log message in one line
	keysAndValues := []interface{}{
		"CPU", strategyConfig.Thresholds[v1.ResourceCPU],
		"Mem", strategyConfig.Thresholds[v1.ResourceMemory],
		"Pods", strategyConfig.Thresholds[v1.ResourcePods],
	}
	for name := range strategyConfig.Thresholds {
		if !isBasicResource(name) {
			keysAndValues = append(keysAndValues, string(name), int64(strategyConfig.Thresholds[name]))
		}
	}

	klog.V(1).InfoS("Criteria for a node below target utilization", keysAndValues...)
	klog.V(1).InfoS("Number of underutilized nodes", "totalNumber", len(sourceNodes))

	if len(sourceNodes) == 0 {
		klog.V(1).InfoS("No node is underutilized, nothing to do here, you might tune your thresholds further")
		return
	}
	if len(sourceNodes) <= strategy.Params.NodeResourceUtilizationThresholds.NumberOfNodes {
		klog.V(1).InfoS("Number of nodes underutilized is less or equal than NumberOfNodes, nothing to do here", "underutilizedNodes", len(sourceNodes), "numberOfNodes", strategy.Params.NodeResourceUtilizationThresholds.NumberOfNodes)
		return
	}
	if len(sourceNodes) == len(nodes) {
		klog.V(1).InfoS("All nodes are underutilized, nothing to do here")
		return
	}
	if len(highNodes) == 0 {
		klog.V(1).InfoS("No node is available to schedule the pods, nothing to do here")
		return
	}

	evictable := podEvictor.Evictable(evictions.WithPriorityThreshold(thresholdPriority), evictions.WithNodeFit(nodeFit))

	// stop if the total available usage has dropped to zero - no more pods can be scheduled
	continueEvictionCond := func(nodeUsage *NodeUsage, totalAvailableUsage map[v1.ResourceName]*resource.Quantity) bool {
		for name := range totalAvailableUsage {
			if totalAvailableUsage[name].CmpInt64(0) < 1 {
				return false
			}
		}

		return true
	}
	evictPodsFromSourceNodes(
		ctx,
		sourceNodes,
		highNodes,
		podEvictor,
		evictable.IsEvictable,
		resourceNames,
		"HighNodeUtilization",
		continueEvictionCond)

}

func validateHighUtilizationStrategyConfig(thresholds *api.NodeResourceUtilizationThresholds) error {
	if len(thresholds.TargetThresholds) != 0 {
		return fmt.Errorf("targetThresholds is not applicable for HighNodeUtilization")
	}
	if err := validateThresholdsHigh(thresholds); err != nil {
		return fmt.Errorf("thresholds config is not valid: %v", err)
	}
	return nil
}

func setDefaultForThresholds(thresholds *api.NodeResourceUtilizationThresholds) {

	// check if Pods/CPU/Mem are set, if not, set them to 100
	if _, ok := thresholds.Thresholds[v1.ResourcePods]; !ok {
		thresholds.Thresholds[v1.ResourcePods] = MaxResourcePercentage
	}
	if _, ok := thresholds.Thresholds[v1.ResourceCPU]; !ok {
		thresholds.Thresholds[v1.ResourceCPU] = MaxResourcePercentage
	}
	if _, ok := thresholds.Thresholds[v1.ResourceMemory]; !ok {
		thresholds.Thresholds[v1.ResourceMemory] = MaxResourcePercentage
	}

	// Default targetThreshold resource values to 100
	thresholds.TargetThresholds[v1.ResourcePods] = MaxResourcePercentage
	thresholds.TargetThresholds[v1.ResourceCPU] = MaxResourcePercentage
	thresholds.TargetThresholds[v1.ResourceMemory] = MaxResourcePercentage

	for name := range thresholds.Thresholds {
		if !isBasicResource(name) {
			thresholds.TargetThresholds[name] = MaxResourcePercentage
		}
	}
}
