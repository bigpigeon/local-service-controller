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

package main

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"time"

	localserviceV1alpha1 "github.com/bigpigeon/local-service-controller/pkg/apis/localservice_controller/v1alpha1"
	clientset "github.com/bigpigeon/local-service-controller/pkg/generated/clientset/versioned"
	localserviceScheme "github.com/bigpigeon/local-service-controller/pkg/generated/clientset/versioned/scheme"
	informers "github.com/bigpigeon/local-service-controller/pkg/generated/informers/externalversions/localservice_controller/v1alpha1"
	listers "github.com/bigpigeon/local-service-controller/pkg/generated/listers/localservice_controller/v1alpha1"
)

const controllerAgentName = "local-service-controller"

const (
	// rack label key
	RackKey = "vm/rack"
	// service owner label key
	OwnerKey = "owner"

	// SuccessSynced is used as part of the Event 'reason' when a LocalService is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a LocalService fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by local-service"
	// MessageResourceSynced is the message used for an Event fired when a local-service
	// is synced successfully
	MessageResourceSynced = "local-service synced successfully"
)

// Controller is the controller implementation for LocalService resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// localServiceClientset is a clientset for our own API group
	localServiceClientset clientset.Interface

	nodeLister         corelisters.NodeLister
	nodeSynced         cache.InformerSynced
	serviceLister      corelisters.ServiceLister
	serviceSynced      cache.InformerSynced
	localserviceLister listers.LocalServiceLister
	localserviceSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new controller
func NewController(
	kubeclientset kubernetes.Interface,
	localServiceClientset clientset.Interface,
	nodeInformer coreinformers.NodeInformer,
	serviceInformer coreinformers.ServiceInformer,
	localServerInformer informers.LocalServiceInformer) *Controller {
	// Create event broadcaster
	utilruntime.Must(localserviceScheme.AddToScheme(scheme.Scheme))
	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:         kubeclientset,
		localServiceClientset: localServiceClientset,
		nodeLister:            nodeInformer.Lister(),
		nodeSynced:            nodeInformer.Informer().HasSynced,
		serviceLister:         serviceInformer.Lister(),
		serviceSynced:         serviceInformer.Informer().HasSynced,
		localserviceLister:    localServerInformer.Lister(),
		localserviceSynced:    localServerInformer.Informer().HasSynced,
		workqueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "LocalServices"),
		recorder:              recorder,
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when LocalService resources change
	localServerInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueLocalServer,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueLocalServer(new)
		},
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.handlerNode,
		DeleteFunc: controller.handlerNode,
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
	klog.Info("Starting LocalService controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.nodeSynced, c.serviceSynced, c.localserviceSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch two workers to process LocalService resources
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
		// LocalService resource to be synced.
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

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the LocalService resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the LocalService resource with this namespace/name
	localService, err := c.localserviceLister.LocalServices(namespace).Get(name)
	if err != nil {
		// The LocalService resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("localService '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	nodes, err := c.nodeLister.List(labels.Set(localService.Spec.NodeSelector).AsSelector())
	if err != nil {
		return err
	}

	myServices, err := c.serviceLister.List(labels.Set{OwnerKey: localService.Name}.AsSelector())
	if err != nil {
		return err
	}
	needDeletedSvc := map[string]*corev1.Service{}
	for _, svc := range myServices {
		needDeletedSvc[svc.Name] = svc
	}

	for _, node := range nodes {
		serviceName := localService.Name + "-" + node.Name
		if _, ok := needDeletedSvc[serviceName]; ok {
			delete(needDeletedSvc, serviceName)
		}
		svc, err := c.serviceLister.Services(localService.Namespace).Get(serviceName)
		if errors.IsNotFound(err) {
			svc, err = c.kubeclientset.CoreV1().Services(localService.Namespace).Create(context.TODO(), newService(localService, node.Name), metav1.CreateOptions{})
		}
		if err != nil {
			return err
		}
		// If the Service is not controlled by this LocalService resource, we should log
		// a warning to the event recorder and return error msg.
		if !metav1.IsControlledBy(svc, localService) {
			msg := fmt.Sprintf(MessageResourceExists, svc.Name)
			c.recorder.Event(localService, corev1.EventTypeWarning, ErrResourceExists, msg)
			return fmt.Errorf(msg)
		}
		// compare service spec
		newSvc := newService(localService, node.Name)
		if svc.Spec.String() != newSvc.Spec.String() {
			svc.Spec = newSvc.Spec
			svc, err = c.kubeclientset.CoreV1().Services(localService.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
	}
	for _, svc := range needDeletedSvc {
		err = c.kubeclientset.CoreV1().Services(localService.Namespace).Delete(context.TODO(), svc.Name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
	}

	c.recorder.Event(localService, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

// enqueueLocalServer takes a LocalService resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than LocalService.
func (c *Controller) enqueueLocalServer(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) handlerNode(obj interface{}) {
	li, err := c.localserviceLister.List(labels.Nothing())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	for _, ls := range li {
		c.workqueue.Add(ls.Name)
	}
}

// newService creates a new Service for a local-service resource. It also sets
// the appropriate OwnerReferences on the resource so handleObject can discover
// the local-service resource that 'owns' it.
func newService(localService *localserviceV1alpha1.LocalService, node string) *corev1.Service {
	spec := localService.Spec.ServiceSpec.DeepCopy()
	spec.Selector[RackKey] = node
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      localService.Name + "-" + node,
			Namespace: localService.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(localService, localserviceV1alpha1.SchemeGroupVersion.WithKind("LocalService")),
			},
			Labels: map[string]string{
				OwnerKey: localService.Name,
			},
		},
		Spec: *spec,
	}
}
