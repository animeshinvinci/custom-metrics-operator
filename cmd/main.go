/*
Copyright 2019 The KubeSphere Authors.

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

package main

import (
	"flag"
	"fmt"
	"github.com/huanggze/custom-metrics-operator/pkg/operator"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	// Uncomment the following line to load the gcp plugin (only required to authenticate against GKE clusters).
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	informers "github.com/coreos/prometheus-operator/pkg/client/informers/externalversions"
	clientset "github.com/coreos/prometheus-operator/pkg/client/versioned"
	"k8s.io/sample-controller/pkg/signals"
)

var (
	ignoredNs ignoreNamespaces
)

type ignoreNamespaces []string

// Set implements the flagset.Value interface.
func (i *ignoreNamespaces) Set(value string) error {
	ignoredNs = strings.Split(value, ",")
	return nil
}

// String implements the flagset.Value interface.
func (i *ignoreNamespaces) String() string {
	return fmt.Sprintf("%v", *i)
}

var (
	kubeconfig  string
	masterURL   string
	operatorCfg operator.Config
)

func init() {
	flagset := flag.CommandLine
	flagset.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flagset.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flagset.StringVar(&operatorCfg.Prometheus, "prometheus", "k8s-system", "The name of Prometheus resource to which ServiceMonitor resources are bound.")
	flagset.StringVar(&operatorCfg.PrometheusNamespace, "prometheus-namespace", "kubesphere-monitoring-system", "The namespace of Prometheus resource.")
	flagset.Var(&ignoredNs, "ignored-namespaces", "Ignore ServiceMonitor resources in specified namespaces.")
	flagset.StringVar(&operatorCfg.ServiceMonitorLabel, "servicemonitor-label", "k8s-app", "The label key that must match the field serviceMonitorSelector of Prometheus resources.")
	flagset.StringVar(&operatorCfg.ServiceMonitorNamespaceLabel, "servicemonitor-namespace-label", "kubesphere.io/workspace", "The label key of ServiceMonitor's namespace.")
	flagset.Parse(os.Args[1:])
	operatorCfg.IgnoredNamespaces = ignoredNs
}

func Main() int {
	klog.InitFlags(nil)

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
		return 1
	}

	monitClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building monitoring clientset: %s", err.Error())
		return 1
	}

	monitInformerFactory := informers.NewSharedInformerFactory(monitClient, time.Minute*30)

	controller := operator.NewController(monitClient, monitInformerFactory.Monitoring().V1().ServiceMonitors(),
		monitInformerFactory.Monitoring().V1().Prometheuses(), operatorCfg)

	// notice that there is no need to run Start methods in a separate goroutine. (i.e. go kubeInformerFactory.Start(stopCh)
	// Start method is non-blocking and runs all registered informers in a dedicated goroutine.
	monitInformerFactory.Start(stopCh)

	if err = controller.Run(2, stopCh); err != nil {
		klog.Fatalf("Error running controller: %s", err.Error())
		return 1
	}

	return 0
}

func main() {
	os.Exit(Main())
}
