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

package operator

import (
	"encoding/json"
	"fmt"
	monitoringv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	informers "github.com/coreos/prometheus-operator/pkg/client/informers/externalversions/monitoring/v1"
	listers "github.com/coreos/prometheus-operator/pkg/client/listers/monitoring/v1"
	clientset "github.com/coreos/prometheus-operator/pkg/client/versioned"
	"github.com/huanggze/custom-metrics-operator/pkg/util"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"reflect"
	"time"
)

const ServiceMonitorFinalizer = "finalizers.kubesphere.io/servicemonitors"

// Controller is the controller implementation for ServiceMonitor resources.
type Controller struct {
	// monitclientset is a clientset for our own API group
	monitclientset clientset.Interface

	smonsLister listers.ServiceMonitorLister
	promsLister listers.PrometheusLister
	smonsSynced cache.InformerSynced
	promsSynced cache.InformerSynced

	cfg Config
	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
}

// Config defines configuration parameters for the Operator.
type Config struct {
	Prometheus                   string
	PrometheusNamespace          string
	IgnoredNamespaces            []string
	ServiceMonitorLabel          string
	ServiceMonitorNamespaceLabel string
}

// NewController returns a new custom metrics operator.
func NewController(
	monitclientset clientset.Interface,
	smonInformer informers.ServiceMonitorInformer,
	promInformer informers.PrometheusInformer,
	conf Config) *Controller {

	controller := &Controller{
		monitclientset: monitclientset,
		smonsLister:    smonInformer.Lister(),
		promsLister:    promInformer.Lister(),
		smonsSynced:    smonInformer.Informer().HasSynced,
		promsSynced:    promInformer.Informer().HasSynced,
		cfg:            conf,
		workqueue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ServiceMonitors"),
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when ServiceMonitor resources change
	smonInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueServiceMonitor,
		UpdateFunc: func(old, new interface{}) {
			newSmon := new.(*monitoringv1.ServiceMonitor)
			oldSmon := old.(*monitoringv1.ServiceMonitor)
			if newSmon.ResourceVersion == oldSmon.ResourceVersion {
				// Periodic resync will send update events for all known ServiceMonitors.
				// Two different versions of the same ServiceMonitor will always have different RVs.
				return
			}
			controller.enqueueServiceMonitor(new)
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting Custom Metrics Operator")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.smonsSynced); !ok {
		return fmt.Errorf("failed to wait for servicemonitors caches to sync")
	}
	if ok := cache.WaitForCacheSync(stopCh, c.promsSynced); !ok {
		return fmt.Errorf("failed to wait for prometheuses caches to sync")
	}

	klog.Info("Starting workers")
	// Launch two workers to process ServiceMonitor resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// ServiceMonitor resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// TODO: add business logic here
// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the ServiceMonitor resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the ServiceMonitor resource with this namespace/name
	smon, err := c.smonsLister.ServiceMonitors(namespace).Get(name)
	if err != nil {
		// The ServiceMonitor resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("servicemonitor '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}
	// Get the label value of the ServiceMonitor resource. The label key is specified in the flag servicemonitor-label
	v, ok := smon.Labels[c.cfg.ServiceMonitorLabel]
	if !ok {
		klog.Warningf("The ServiceMonitor '%s' doesn't contain label %s", key, c.cfg.ServiceMonitorLabel)
		return nil
	}
	// Get the Prometheus resource with this namespace/name
	prom, err := c.promsLister.Prometheuses(c.cfg.PrometheusNamespace).Get(c.cfg.Prometheus)
	if err != nil {
		// The Prometheus resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("prometheus '%s/%s' is not found", c.cfg.PrometheusNamespace, c.cfg.Prometheus))
			return nil
		}

		return err
	}

	// Use finalizers to implement asynchronous pre-delete hooks
	if smon.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object.
		if !util.ContainsString(smon.ObjectMeta.Finalizers, ServiceMonitorFinalizer) {
			smon.ObjectMeta.Finalizers = append(smon.ObjectMeta.Finalizers, ServiceMonitorFinalizer)
			if _, err := c.monitclientset.MonitoringV1().ServiceMonitors(namespace).Update(smon); err != nil {
				return err
			}
		}
	} else {
		// The object is being deleted
		if util.ContainsString(smon.ObjectMeta.Finalizers, ServiceMonitorFinalizer) {
			// remove our finalizer from the list and update it.
			smon.ObjectMeta.Finalizers = util.RemoveString(smon.ObjectMeta.Finalizers, ServiceMonitorFinalizer)
			if _, err := c.monitclientset.MonitoringV1().ServiceMonitors(namespace).Update(smon); err != nil {
				return err
			}

			for i, exp := range prom.Spec.ServiceMonitorSelector.MatchExpressions {
				if exp.Key == c.cfg.ServiceMonitorLabel && exp.Operator == metav1.LabelSelectorOpIn {
					prom.Spec.ServiceMonitorSelector.MatchExpressions[i].Values = util.RemoveString(prom.Spec.ServiceMonitorSelector.MatchExpressions[i].Values, v)
					// Update prometheus via patch
					payloadBytes, err := json.Marshal(prom)
					if err != nil {
						return err
					}
					_, err = c.monitclientset.MonitoringV1().Prometheuses(prom.Namespace).Patch(prom.Name, types.MergePatchType, payloadBytes)
					if err != nil {
						return err
					}
					break
				}
			}

			return nil
		}
	}

	// Set namespace label requirements for ServiceMonitorNamespaceSelector
	smonNamespaceSelector := metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      c.cfg.ServiceMonitorNamespaceLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}
	if !reflect.DeepEqual(prom.Spec.ServiceMonitorNamespaceSelector.MatchExpressions, smonNamespaceSelector.MatchExpressions) {
		prom.Spec.ServiceMonitorNamespaceSelector.MatchExpressions = smonNamespaceSelector.MatchExpressions
		prom.Spec.ServiceMonitorNamespaceSelector.MatchLabels = nil
	}

	// Bound the specific ServiceMonitor resource to Prometheus via ServiceMonitorSelector
	for i, exp := range prom.Spec.ServiceMonitorSelector.MatchExpressions {
		if exp.Key == c.cfg.ServiceMonitorLabel && exp.Operator == metav1.LabelSelectorOpIn {
			prom.Spec.ServiceMonitorSelector.MatchExpressions[i].Values = util.AppendIfUnique(prom.Spec.ServiceMonitorSelector.MatchExpressions[i].Values, v)
			break
		}

		if i == len(prom.Spec.ServiceMonitorSelector.MatchExpressions) {
			prom.Spec.ServiceMonitorSelector.MatchExpressions = metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      c.cfg.ServiceMonitorLabel,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{v},
					},
				},
			}.MatchExpressions
		}
	}

	// Update prometheus via patch
	payloadBytes, err := json.Marshal(prom)
	if err != nil {
		return err
	}
	_, err = c.monitclientset.MonitoringV1().Prometheuses(prom.Namespace).Patch(prom.Name, types.MergePatchType, payloadBytes)
	if err != nil {
		return err
	}

	return nil
}

// enqueueServiceMonitor takes a ServiceMonitor resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than ServiceMonitor.
func (c *Controller) enqueueServiceMonitor(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}

	// No need to enqueue ServiceMonitor resources from IgnoreNamespaces
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	if util.ContainsString(c.cfg.IgnoredNamespaces, namespace) {
		return
	}

	c.workqueue.Add(key)
}
