/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an Sub"AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/containerd/nri/v2alpha1/pkg/api"
	"github.com/containerd/nri/v2alpha1/pkg/stub"
)

type plugin struct {
	stub      stub.Stub
	podData   map[string]string
	clientset *kubernetes.Clientset
}

func (p *plugin) Configure(nriCfg string) (stub.SubscribeMask, error) {
	return stub.RunPodSandbox | stub.CreateContainer, nil
}

func (p *plugin) RunPodSandbox(pod *api.PodSandbox) {
	var opts metav1.GetOptions
	fmt.Printf("RunPodSandbox: pod=%s/%s\n", pod.Namespace, pod.Name)
	k8spod, err := p.clientset.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, opts)
	if err == nil {
		k8spodjson, _ := json.Marshal(k8spod)
		p.podData[pod.Namespace + "/" + pod.Name] = string(k8spodjson)
		fmt.Printf("%s\n", k8spodjson)
	} else {
		fmt.Printf("get failed: %v\n", err)
	}
}

func (p *plugin) CreateContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerCreateAdjustment, []*api.ContainerAdjustment, error) {
	if data, ok := p.podData[pod.Namespace + "/" + pod.Name]; ok {
		adj := &api.ContainerCreateAdjustment{
			Annotations: container.Annotations,
		}
		adj.Annotations["poddata"] = data
		return adj, nil, nil
	}
	return nil, nil, nil
}

/// There should be a way to avoid listing parts of the API that are
/// not interesting.
/// Maybe something like:
/// p = stub.NewSimple(stub.EventMap{stub.RunPodSandbox: RunPodSandbox})
/// ---8<---
func (p *plugin) Synchronize(pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerAdjustment, error) {
	return nil, nil
}

func (p *plugin) Shutdown() {
}

func (p *plugin) StopPodSandbox(pod *api.PodSandbox) {
}

func (p *plugin) RemovePodSandbox(pod *api.PodSandbox) {
}

func (p *plugin) PostCreateContainer(pod *api.PodSandbox, container *api.Container) {
}

func (p *plugin) StartContainer(pod *api.PodSandbox, container *api.Container) {
}

func (p *plugin) PostStartContainer(pod *api.PodSandbox, container *api.Container) {
}

func (p *plugin) UpdateContainer(pod *api.PodSandbox, container *api.Container) ([]*api.ContainerAdjustment, error) {
	return nil, nil
}

func (p *plugin) PostUpdateContainer(pod *api.PodSandbox, container *api.Container) {
}

func (p *plugin) StopContainer(pod *api.PodSandbox, container *api.Container) ([]*api.ContainerAdjustment, error) {
	return nil, nil
}

func (p *plugin) RemoveContainer(pod *api.PodSandbox, container *api.Container) {
}

/// --->8---

func (p *plugin) Exit() {
	os.Exit(0)
}

func Error(format string, args ...interface{}) {
	fmt.Fprintln(os.Stderr, fmt.Sprintf("nri-podresources: "+format, args...))
	os.Exit(1)
}

func main() {
	// connect to kubernetes
	var kubeconfig *string
	kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	flag.Parse()
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// create plugin
	p := &plugin{
		clientset: clientset,
		podData: map[string]string{},
	}
	s, err := stub.New(p, stub.WithOnClose(p.Exit))
	if err != nil {
		Error("failed to create plugin stub: %v", err)
	}
	p.stub = s

	err = p.stub.Run(context.Background())
	if err != nil {
		Error("Plugin exited with error %v", err)
	}
}
