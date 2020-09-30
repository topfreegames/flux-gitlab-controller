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
	"crypto/rsa"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xanzy/go-gitlab"
	"golang.org/x/crypto/ssh"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	v1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const controllerAgentName = "flux-gitlab-controller"

const (

	// deployKeyLabelName is the label used to update the secret with the gitlab
	// deploy key id
	deployKeyLabelName = "fluxcd.io/deployKeyId"

	// gitUrlLabelName is the label used to retrieve the gitlab project url used to
	// add the deployment key to
	fluxSecretLabelFilter = "fluxcd.io/sync-gc-mark"

	// gitUrlLabelName is the label used to retrieve the gitlab project url used to
	// add the deployment key to
	gitUrlLabelName = "fluxcd.io/git-url"

	// SuccessSynced is used as part of the Event 'reason' when a Secret is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Secret fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Secret"
	// MessageResourceSynced is the message used for an Event fired when a Secret
	// is synced successfully
	MessageResourceSynced = "Secret synced successfully"
)

// Controller is the controller implementation for Secret resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	secretsLister corelisters.SecretLister
	secretsSynced cache.InformerSynced

	gitlabClient *gitlab.Client

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

// NewController returns a new sample controller
func NewController(
	kubeclientset kubernetes.Interface,
	secretInformer v1.SecretInformer) *Controller {

	// Create event broadcaster
	// Add Flux controller types to the default Kubernetes Scheme so Events can be
	// logged for controller types.
	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()

	gitlabClient, _ := gitlab.NewClient(gitlabToken, gitlab.WithBaseURL(fmt.Sprintf("https://%s/api/v4", gitlabHostname)))

	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset: kubeclientset,
		secretsLister: secretInformer.Lister(),
		secretsSynced: secretInformer.Informer().HasSynced,
		workqueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Secrets"),
		gitlabClient:  gitlabClient,
		recorder:      recorder,
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when Flux secret changes resources change

	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
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
	klog.Info("Starting Secret controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.secretsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch two workers to process Secret resources
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
		var key *corev1.Secret
		var ok bool
		// We expect Secrets to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(*corev1.Secret); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Secret resource to be synced.
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
// converge the two. It then updates the deployKeyId block of the Secret resource
// with the current status of the resource.
func (c *Controller) syncHandler(secret *corev1.Secret) error {

	// Get the Secret resource with this namespace/name
	_, err := c.secretsLister.Secrets(secret.Namespace).Get(secret.Name)
	if err != nil {
		// The Secret resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("secret '%s' in work queue no longer exists", secret.Name))
			deployKey, err := strconv.Atoi(secret.Annotations[deployKeyLabelName])
			if err != nil {
				return err
			}
			klog.V(4).Infof("Deleting deploy key %d", deployKey)
			project := strings.TrimPrefix(secret.Annotations[gitUrlLabelName], fmt.Sprintf("git@%s:", gitlabHostname))
			// Removes .git in the URL if present
			projectBase := strings.TrimSuffix(project, ".git")

			c.gitlabClient.DeployKeys.DeleteDeployKey(projectBase, deployKey)
			return nil
		}

		return err
	}

	if _, ok := secret.Annotations[gitUrlLabelName]; ok {
		klog.V(4).Infof("Secret %s is not a flux secret", secret.GetName())
		return nil
	}

	// We could make the controller check if the key exist in the gitlab API
	// and re-create it if missing but I'm a bit concerned about the amount of
	// pressure that it could put into the API
	if _, ok := secret.Annotations[deployKeyLabelName]; ok {
		klog.V(4).Infof("Secret %s already has deployKey, no need to update", secret.GetName())
		return nil
	}

	project := strings.TrimPrefix(secret.Annotations[gitUrlLabelName], fmt.Sprintf("git@%s:", gitlabHostname))
	// Removes .git in the URL if present
	projectBase := strings.TrimSuffix(project, ".git")

	p, _, err := c.gitlabClient.Projects.GetProject(projectBase, nil)

	if err != nil {
		return err
	}

	repoKey := secret.Data["identity"]
	k, err := ssh.ParseRawPrivateKey(repoKey)

	if err != nil {
		return err
	}

	rsaKey, _ := k.(*rsa.PrivateKey)

	sshKey, err := ssh.NewPublicKey(rsaKey.Public())

	if err != nil {
		return err
	}

	keyResp, _, err := c.gitlabClient.DeployKeys.AddDeployKey(p.ID, &gitlab.AddDeployKeyOptions{Title: gitlab.String("Flux deployment key"), Key: gitlab.String(string(ssh.MarshalAuthorizedKey(sshKey))), CanPush: gitlab.Bool(true)})
	if err != nil {
		return err
	}
	klog.V(4).Infof("Adding deploy key %d", keyResp.ID)

	// Finally, we update the status block of the Secret resource to reflect the
	// current state of the world
	err = c.updateSecretStatus(secret, strconv.Itoa(keyResp.ID))
	if err != nil {
		return err
	}

	c.recorder.Event(secret, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (c *Controller) updateSecretStatus(secret *corev1.Secret, deployKeyId string) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	secretCopy := secret.DeepCopy()
	secretCopy.Annotations[deployKeyLabelName] = deployKeyId
	// If the CustomResourceSubresources feature gate is not enabled,
	// we must use Update instead of UpdateStatus to update the Status block of the Secret resource.
	// UpdateStatus will not allow changes to the Spec of the resource,
	// which is ideal for ensuring nothing other than resource status has been updated.
	_, err := c.kubeclientset.CoreV1().Secrets(secret.Namespace).Update(context.TODO(), secretCopy, metav1.UpdateOptions{})
	return err
}

// enqueue takes a Secret resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Secret.
func (c *Controller) enqueue(obj interface{}) {
	c.workqueue.Add(obj)
}

// It enqueues the Secret resource to be processed.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		klog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}

	klog.V(4).Infof("Processing object: %s", object.GetName())
	c.enqueue(obj)
}
