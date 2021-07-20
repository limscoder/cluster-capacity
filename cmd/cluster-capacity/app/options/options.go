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

package options

import (
	"fmt"
	"io/ioutil"
	"k8s.io/api/extensions/v1beta1"
	"os"
	"path/filepath"
	"sigs.k8s.io/cluster-capacity/pkg/framework"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientset "k8s.io/client-go/kubernetes"
	api "k8s.io/kubernetes/pkg/apis/core"
	apiv1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/apis/core/validation"
)

const INPUT_SEPARATOR = ":"
const YAML_SEPARATOR = "---"

type ClusterCapacityConfig struct {
	ReplicatedPods []*framework.ReplicatedPod
	SimulatedNodes []*framework.SimulatedNode
	KubeClient     clientset.Interface
	Options        *ClusterCapacityOptions
}

type ClusterCapacityOptions struct {
	Kubeconfig                 string
	DefaultSchedulerConfigFile string
	Verbose                    bool
	ReplicaSetFiles            []string
	SimulatedNodes             []string
	NodeLabels                 []string
	Namespace                  string
	OutputFormat               string
}

func NewClusterCapacityConfig(opt *ClusterCapacityOptions) *ClusterCapacityConfig {
	return &ClusterCapacityConfig{
		Options: opt,
	}
}

func NewClusterCapacityOptions() *ClusterCapacityOptions {
	return &ClusterCapacityOptions{Namespace: "default"}
}

func (s *ClusterCapacityOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&s.Kubeconfig, "kubeconfig", s.Kubeconfig, "Path to the kubeconfig file to use for the analysis.")
	fs.StringArrayVar(&s.ReplicaSetFiles, "replicaset", s.ReplicaSetFiles, "Path to JSON or YAML file containing replicaset definition.")
	fs.StringArrayVar(&s.SimulatedNodes, "simulatenode", s.SimulatedNodes, "Simulate additional cluster nodes. Replicate a node by specifying a source node and count {SOURCE_NODE_NAME}:{SIMULATED_COUNT}.")
	fs.StringArrayVar(&s.NodeLabels, "nodelabel", s.NodeLabels, "Aggregate cluster capacity by node label.")

	fs.StringVar(&s.Namespace, "namespace", s.Namespace, "Namespace to schedule replicasets in.")

	//TODO(jchaloup): uncomment this line once the multi-schedulers are fully implemented
	//fs.StringArrayVar(&s.SchedulerConfigFile, "config", s.SchedulerConfigFile, "Paths to files containing scheduler configuration in JSON or YAML format")

	fs.StringVar(&s.DefaultSchedulerConfigFile, "default-config", s.DefaultSchedulerConfigFile, "Path to JSON or YAML file containing scheduler configuration.")

	fs.BoolVar(&s.Verbose, "verbose", s.Verbose, "Verbose mode")
	fs.StringVarP(&s.OutputFormat, "output", "o", s.OutputFormat, "Output format. One of: json|yaml (Note: output is not versioned or guaranteed to be stable across releases).")
}

func (s *ClusterCapacityConfig) ParseAPISpec(schedulerName string) error {
	replicaCfgs, err := s.replicaConfigs(schedulerName)
	if err != nil {
		return err
	}
	s.ReplicatedPods = replicaCfgs
	nodeCfgs, err := s.nodeConfigs()
	if err != nil {
		return err
	}
	s.SimulatedNodes = nodeCfgs
	return nil
}

func (s *ClusterCapacityConfig) replicaConfigs(schedulerName string) ([]*framework.ReplicatedPod, error) {
	replicatedPods := make([]*framework.ReplicatedPod, 0)
	for _, replicaSetFile := range s.Options.ReplicaSetFiles {
		filename, _ := filepath.Abs(replicaSetFile)
		spec, err := os.Open(filename)
		if err != nil {
			return nil, fmt.Errorf("Failed to open replicaset file: %v", err)
		}
		content, err := ioutil.ReadAll(spec)
		if err != nil {
			return nil, fmt.Errorf("Failed to ready replicaset file: %v", err)
		}
		documents := strings.Split(string(content), YAML_SEPARATOR)
		for _, doc := range documents {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}
			decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(doc), 4096)
			replicaSet := &v1beta1.ReplicaSet{}
			err = decoder.Decode(replicaSet)
			if err != nil {
				return nil, fmt.Errorf("Failed to decode replicaset file: %v", err)
			}
			pod, err := s.newReplicatedPod(schedulerName, replicaSet)
			if err != nil {
				return nil, err
			}
			replicatedPods = append(replicatedPods, pod)
		}
	}

	return replicatedPods, nil
}

func (s *ClusterCapacityConfig) newReplicatedPod(schedulerName string, replicaSet *v1beta1.ReplicaSet) (*framework.ReplicatedPod, error) {
	pod := &v1.Pod{
		ObjectMeta: replicaSet.Spec.Template.ObjectMeta,
		Spec:       replicaSet.Spec.Template.Spec,
	}
	if pod.Name == "" {
		pod.Name = replicaSet.Name
	}
	pod.Namespace = s.Options.Namespace

	// set pod's scheduler name to cluster-capacity
	if pod.Spec.SchedulerName == "" {
		pod.Spec.SchedulerName = schedulerName
	}

	// hardcoded from kube api defaults and validation
	// TODO: rewrite when object validation gets more available for non kubectl approaches in kube
	if pod.Spec.DNSPolicy == "" {
		pod.Spec.DNSPolicy = v1.DNSClusterFirst
	}
	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = v1.RestartPolicyAlways
	}

	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].TerminationMessagePolicy == "" {
			pod.Spec.Containers[i].TerminationMessagePolicy = v1.TerminationMessageFallbackToLogsOnError
		}
	}

	// TODO: client side validation seems like a long term problem for this command.
	internalPod := &api.Pod{}
	if err := apiv1.Convert_v1_Pod_To_core_Pod(pod, internalPod, nil); err != nil {
		return nil, fmt.Errorf("unable to convert to internal version: %#v", err)

	}
	if errs := validation.ValidatePodCreate(internalPod, validation.PodValidationOptions{}); len(errs) > 0 {
		var errStrs []string
		for _, err := range errs {
			errStrs = append(errStrs, fmt.Sprintf("%v: %v", err.Type, err.Field))
		}
		return nil, fmt.Errorf("Invalid pod: %#v", strings.Join(errStrs, ", "))
	}

	replicas := 0
	if replicaSet.Spec.Replicas != nil {
		replicas = int(*replicaSet.Spec.Replicas)
	}
	return &framework.ReplicatedPod{
		Replicas:      replicas,
		Pod:           pod,
		ScheduledPods: []*v1.Pod{},
	}, nil
}

func (s *ClusterCapacityConfig) nodeConfigs() ([]*framework.SimulatedNode, error) {
	nodes := make([]*framework.SimulatedNode, len(s.Options.SimulatedNodes), len(s.Options.SimulatedNodes))
	for i, node := range s.Options.SimulatedNodes {
		parts := strings.Split(node, INPUT_SEPARATOR)
		if len(parts) != 2 {
			return nil, fmt.Errorf("Invalid simulated node input format: %s", node)
		}

		replicas, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}

		nodes[i] = &framework.SimulatedNode{
			NodeName: parts[0],
			Replicas: replicas,
		}
	}
	return nodes, nil
}
