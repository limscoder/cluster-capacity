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

package framework

import (
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	v1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
)

const (
	ResourceNvidiaGPU v1.ResourceName = "nvdia.com/gpu"
)

type ClusterCapacityReview struct {
	metav1.TypeMeta
	Spec   ClusterCapacityReviewSpec   `json:"spec"`
	Status ClusterCapacityReviewStatus `json:"status"`
}

type ClusterCapacityReviewSpec struct {
	// the pod desired for scheduling
	Templates []v1.Pod `json:"templates"`

	PodRequirements []*Requirements `json:"podRequirements"`
}

type ClusterCapacityReviewStatus struct {
	CreationTimestamp time.Time `json:"creationTimestamp"`
	// actual number of replicas that could schedule
	Replicas int32 `json:"replicas"`

	FailReason *ClusterCapacityReviewScheduleFailReason `json:"failReason"`

	// per node information about the scheduling simulation
	Pods  []*ClusterCapacityPodResult  `json:"pods"`
	Nodes []*ClusterCapacityNodeResult `json:"nodes"`
}

type PodReplicaCount map[string]int

type ClusterCapacityPodResult struct {
	PodName string `json:"podName"`
	// numbers of replicas on nodes
	ReplicasOnNodes PodReplicaCount `json:"replicasOnNodes"`
	// reason why no more pods could schedule (if any on this node)
	FailSummary []FailReasonSummary `json:"failSummary"`
}

type NodeMap map[string]*framework.NodeInfo

type ClusterCapacityNodeResult struct {
	NodeName    string              `json:"nodeName"`
	Labels      map[string]string   `json:"labels"`
	PodCount    int                 `json:"podCount"`
	Allocatable *framework.Resource `json:"allocatable"`
	Requested   *framework.Resource `json:"requested"`
	Limits      *framework.Resource `json:"limits"`
}

type FailReasonSummary struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type Resources struct {
	PrimaryResources v1.ResourceList           `json:"primaryResources"`
	ScalarResources  map[v1.ResourceName]int64 `json:"scalarResources"`
}

type Requirements struct {
	PodName       string            `json:"podName"`
	Resources     *Resources        `json:"resources"`
	Limits        *Resources        `json:"limits"`
	NodeSelectors map[string]string `json:"nodeSelectors"`
}

type ClusterCapacityReviewScheduleFailReason struct {
	FailType    string `json:"failType"`
	FailMessage string `json:"failMessage"`
}

func getMainFailReason(message string) *ClusterCapacityReviewScheduleFailReason {
	slicedMessage := strings.Split(message, "\n")
	colon := strings.Index(slicedMessage[0], ":")

	fail := &ClusterCapacityReviewScheduleFailReason{
		FailType:    slicedMessage[0][:colon],
		FailMessage: strings.Trim(slicedMessage[0][colon+1:], " "),
	}
	return fail
}

func getResourceRequest(pod *v1.Pod) *Resources {
	result := newResources()
	for _, container := range pod.Spec.Containers {
		appendResources(result, container.Resources.Requests)
	}
	return result
}

func getResourceLimit(pod *v1.Pod) *Resources {
	result := newResources()
	for _, container := range pod.Spec.Containers {
		appendResources(result, container.Resources.Limits)
	}
	return result
}

func newResources() *Resources {
	return &Resources{
		PrimaryResources: v1.ResourceList{
			v1.ResourceName(v1.ResourceCPU):              *resource.NewMilliQuantity(0, resource.DecimalSI),
			v1.ResourceName(v1.ResourceMemory):           *resource.NewQuantity(0, resource.BinarySI),
			v1.ResourceName(v1.ResourceEphemeralStorage): *resource.NewQuantity(0, resource.BinarySI),
			v1.ResourceName(ResourceNvidiaGPU):           *resource.NewMilliQuantity(0, resource.DecimalSI),
		},
	}
}

func appendResources(dest *Resources, src v1.ResourceList) {
	for rName, rQuantity := range src {
		switch rName {
		case v1.ResourceMemory:
			rQuantity.Add(*(dest.PrimaryResources.Memory()))
			dest.PrimaryResources[v1.ResourceMemory] = rQuantity
		case v1.ResourceCPU:
			rQuantity.Add(*(dest.PrimaryResources.Cpu()))
			dest.PrimaryResources[v1.ResourceCPU] = rQuantity
		case v1.ResourceEphemeralStorage:
			rQuantity.Add(*(dest.PrimaryResources.StorageEphemeral()))
			dest.PrimaryResources[v1.ResourceEphemeralStorage] = rQuantity
		case v1.ResourceStorage:
			rQuantity.Add(*(dest.PrimaryResources.Storage()))
			dest.PrimaryResources[v1.ResourceStorage] = rQuantity
			//case v1.ResourceNvidiaGPU:
			//	rQuantity.Add(*(result.PrimaryResources.NvidiaGPU()))
			//	result.PrimaryResources[v1.ResourceNvidiaGPU] = rQuantity
		default:
			if schedutil.IsScalarResourceName(rName) {
				// Lazily allocate this map only if required.
				if dest.ScalarResources == nil {
					dest.ScalarResources = map[v1.ResourceName]int64{}
				}
				dest.ScalarResources[rName] += rQuantity.Value()
			}
		}
	}
}

func parseNodesReview(nodes NodeMap) []*ClusterCapacityNodeResult {
	// sort nodes by name
	nodeNames := make([]string, len(nodes), len(nodes))
	nodeIdx := 0
	for key, _ := range nodes {
		nodeNames[nodeIdx] = key
		nodeIdx++
	}
	sort.Strings(nodeNames)

	result := make([]*ClusterCapacityNodeResult, len(nodes), len(nodes))
	for i, key := range nodeNames {
		node := nodes[key]
		limits := newResources()
		for _, pod := range node.Pods {
			appendResources(limits, getResourceLimit(pod.Pod).PrimaryResources)
		}
		result[i] = &ClusterCapacityNodeResult{
			NodeName:    key,
			Labels:      node.Node().Labels,
			PodCount:    len(node.Pods),
			Allocatable: node.Allocatable,
			Requested:   node.Requested,
			Limits: &framework.Resource{
				MilliCPU:         limits.PrimaryResources.Cpu().MilliValue(),
				Memory:           limits.PrimaryResources.Memory().Value(),
				EphemeralStorage: limits.PrimaryResources.StorageEphemeral().Value(),
				ScalarResources:  limits.ScalarResources,
			},
		}
	}
	return result
}

func parsePodsReview(templatePods []*v1.Pod, status Status) []*ClusterCapacityPodResult {
	results := map[string]*ClusterCapacityPodResult{}
	for _, tmpl := range templatePods {
		results[tmpl.Name] = &ClusterCapacityPodResult{
			ReplicasOnNodes: PodReplicaCount{},
			PodName:         tmpl.Name,
		}
	}

	for _, pod := range status.Pods {
		tmplName, tFound := pod.ObjectMeta.Annotations[podTemplate]
		if !tFound {
			log.Fatal(fmt.Errorf("pod template annotation missing"))
		}
		result, rFound := results[tmplName]
		if !rFound {
			log.Fatal(fmt.Errorf("unknown pod template: %s", tmplName))
		}

		result.ReplicasOnNodes[pod.Spec.NodeName]++
	}

	resultSlc := make([]*ClusterCapacityPodResult, 0)
	for _, v := range results {
		resultSlc = append(resultSlc, v)
	}
	return resultSlc
}

func getPodsRequirements(pods []*v1.Pod) []*Requirements {
	result := make([]*Requirements, 0)
	for _, pod := range pods {
		podRequirements := &Requirements{
			PodName:       pod.Name,
			Resources:     getResourceRequest(pod),
			Limits:        getResourceLimit(pod),
			NodeSelectors: pod.Spec.NodeSelector,
		}
		result = append(result, podRequirements)
	}
	return result
}

func deepCopyPods(in []*v1.Pod, out []v1.Pod) {
	for i, pod := range in {
		out[i] = *pod.DeepCopy()
	}
}

func getReviewSpec(podTemplates []*v1.Pod) ClusterCapacityReviewSpec {
	podCopies := make([]v1.Pod, len(podTemplates))
	deepCopyPods(podTemplates, podCopies)
	return ClusterCapacityReviewSpec{
		Templates:       podCopies,
		PodRequirements: getPodsRequirements(podTemplates),
	}
}

func getReviewStatus(pods []*v1.Pod, nodes NodeMap, status Status) ClusterCapacityReviewStatus {
	return ClusterCapacityReviewStatus{
		CreationTimestamp: time.Now(),
		Replicas:          int32(len(status.Pods)),
		FailReason:        getMainFailReason(status.StopReason),
		Pods:              parsePodsReview(pods, status),
		Nodes:             parseNodesReview(nodes),
	}
}

func GetReport(pods []*v1.Pod, nodes NodeMap, status Status) *ClusterCapacityReview {
	return &ClusterCapacityReview{
		Spec:   getReviewSpec(pods),
		Status: getReviewStatus(pods, nodes, status),
	}
}

func instancesSum(replicasOnNodes PodReplicaCount) int {
	result := 0
	for _, v := range replicasOnNodes {
		result += v
	}
	return result
}

func clusterCapacityReviewPrettyPrint(r *ClusterCapacityReview, nodeLabels []string, verbose bool) {
	if verbose {
		fmt.Println("========== Simulation spec")
		for _, req := range r.Spec.PodRequirements {
			fmt.Printf("%v\n", req.PodName)
			fmt.Printf("\trequests:\n")
			printResources(req.Resources)
			fmt.Printf("\tlimits:\n")
			printResources(req.Limits)

			if req.NodeSelectors != nil {
				fmt.Printf("\t- NodeSelector: %v\n", labels.SelectorFromSet(labels.Set(req.NodeSelectors)).String())
			}
			fmt.Println("========== Simulation result")
		}
	}

	for _, pod := range r.Status.Pods {
		if verbose {
			fmt.Printf("The cluster can schedule %v instance(s) of the pod %v.\n", instancesSum(pod.ReplicasOnNodes), pod.PodName)
		} else {
			fmt.Printf("%v\n", instancesSum(pod.ReplicasOnNodes))
		}
	}

	if verbose {
		fmt.Printf("\nTermination reason: %v: %v\n", r.Status.FailReason.FailType, r.Status.FailReason.FailMessage)
	}

	if verbose && r.Status.Replicas > 0 {
		for _, pod := range r.Status.Pods {
			if pod.FailSummary != nil {
				fmt.Printf("fit failure summary on nodes: ")
				for _, fs := range pod.FailSummary {
					fmt.Printf("%v (%v), ", fs.Reason, fs.Count)
				}
				fmt.Printf("\n")
			}
		}
		fmt.Printf("\nPod distribution among nodes:\n")
		for _, pod := range r.Status.Pods {
			fmt.Printf("%v\n", pod.PodName)
			for node, count := range pod.ReplicasOnNodes {
				fmt.Printf("\t- %v: %v instance(s)\n", node, count)
			}
		}
		printNodeCapacity(r.Status.Nodes)
		printClusterCapacity("========== Cluster capacity", r.Status.Nodes)
		printLabeledCapacity(nodeLabels, r.Status.Nodes)
	}
}
func printLabeledCapacity(nodeLabels []string, nodes []*ClusterCapacityNodeResult) {
	labeledResults := map[string][]*ClusterCapacityNodeResult{}
	for _, node := range nodes {
		for _, label := range nodeLabels {
			value, ok := node.Labels[label]
			if !ok {
				continue
			}

			resultName := fmt.Sprintf("%s:%s", label, value)
			labeledResults[resultName] = append(labeledResults[resultName], node)
		}
	}
	for label, results := range labeledResults {
		printClusterCapacity(label, results)
	}
}

func printClusterCapacity(title string, nodes []*ClusterCapacityNodeResult) {
	var (
		clusterCPUAllocatable, clusterCPURequested, clusterCPULimit,
		clusterMemoryAllocatable, clusterMemoryRequested, clusterMemoryLimit,
		clusterStorageAllocatable, clusterStorageRequested, clusterStorageLimit int64
	)

	for _, node := range nodes {
		clusterCPUAllocatable += node.Allocatable.MilliCPU
		clusterCPURequested += node.Requested.MilliCPU
		clusterCPULimit += node.Limits.MilliCPU
		clusterMemoryAllocatable += node.Allocatable.Memory
		clusterMemoryRequested += node.Requested.Memory
		clusterMemoryLimit += node.Limits.Memory
		clusterStorageAllocatable += node.Allocatable.EphemeralStorage
		clusterStorageRequested += node.Requested.EphemeralStorage
		clusterStorageLimit += node.Limits.EphemeralStorage
	}

	fmt.Printf("\n%s:\n", title)
	printCapacity(clusterCPUAllocatable, clusterCPURequested, clusterCPULimit, "CPU", "m")
	printCapacity(clusterMemoryAllocatable, clusterMemoryRequested, clusterMemoryLimit, "Memory", "bytes")
	printCapacity(clusterStorageAllocatable, clusterStorageRequested, clusterStorageLimit, "EphemeralStorage", "bytes")
}

func printNodeCapacity(nodes []*ClusterCapacityNodeResult) {
	fmt.Printf("\n========== Node capacity:\n")
	for _, node := range nodes {
		fmt.Printf("%s\n", node.NodeName)
		fmt.Printf("\t- pod count: %v\n", node.PodCount)

		printCapacity(node.Allocatable.MilliCPU, node.Requested.MilliCPU, node.Limits.MilliCPU, "CPU", "m")
		printCapacity(node.Allocatable.Memory, node.Requested.Memory, node.Limits.Memory, "Memory", "bytes")
		printCapacity(node.Allocatable.EphemeralStorage, node.Requested.EphemeralStorage, node.Limits.EphemeralStorage, "EphemeralStorage", "bytes")
	}
}

func printCapacity(allocatable, requested, limit int64, label, unit string) {
	cap := float64(requested) / float64(allocatable) * 100
	fmt.Printf("\t- %s requested: %v%s/%v%s %.2f%% allocated\n",
		label, requested, unit, allocatable, unit, cap)
	commit := float64(limit) / float64(allocatable) * 100
	fmt.Printf("\t- %s limited: %v%s/%v%s %.2f%% allocated\n",
		label, limit, unit, allocatable, unit, commit)
}

func printResources(resources *Resources) {
	fmt.Printf("\t\t- CPU: %v\n", resources.PrimaryResources.Cpu().String())
	fmt.Printf("\t\t- Memory: %v\n", resources.PrimaryResources.Memory().String())

	if resources.PrimaryResources.StorageEphemeral() != nil {
		fmt.Printf("\t\t- Ephemeral Storage: %v\n", resources.PrimaryResources.StorageEphemeral().String())
	}

	//if !req.Resources.PrimaryResources.NvidiaGPU().IsZero() {
	//	fmt.Printf("\t- NvidiaGPU: %v\n", req.Resources.PrimaryResources.NvidiaGPU().String())
	//}
	if resources.ScalarResources != nil {
		fmt.Printf("\t\t- ScalarResources: %v\n", resources.ScalarResources)
	}
}

func clusterCapacityReviewPrintJson(r *ClusterCapacityReview) error {
	jsoned, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("Failed to create json: %v", err)
	}
	fmt.Println(string(jsoned))
	return nil
}

func clusterCapacityReviewPrintYaml(r *ClusterCapacityReview) error {
	yamled, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("Failed to create yaml: %v", err)
	}
	fmt.Print(string(yamled))
	return nil
}

func ClusterCapacityReviewPrint(r *ClusterCapacityReview, nodeLabels []string, verbose bool, format string) error {
	switch format {
	case "json":
		return clusterCapacityReviewPrintJson(r)
	case "yaml":
		return clusterCapacityReviewPrintYaml(r)
	case "":
		clusterCapacityReviewPrettyPrint(r, nodeLabels, verbose)
		return nil
	default:
		return fmt.Errorf("output format %q not recognized", format)
	}
}
