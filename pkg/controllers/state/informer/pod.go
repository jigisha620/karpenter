/*
Copyright The Kubernetes Authors.

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

package informer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	utilscontroller "sigs.k8s.io/karpenter/pkg/utils/controller"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const (
	minReconciles = 10
	maxReconciles = 3000
)

var stateRetryPeriod = 1 * time.Minute

// PodController reconciles pods for the purpose of maintaining state regarding pods that is expensive to compute.
type PodController struct {
	kubeClient client.Client
	cluster    *state.Cluster
}

func NewPodController(kubeClient client.Client, cluster *state.Cluster) *PodController {
	return &PodController{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *PodController) Name() string {
	return "state.pod"
}

func (c *PodController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())

	pod := &v1.Pod{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			// notify cluster state of the node deletion
			c.cluster.DeletePod(req.NamespacedName)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if err := c.cluster.UpdatePod(ctx, pod); err != nil {
		// We requeue here since the NotFound error is from finding the node for the binding
		if errors.IsNotFound(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: stateRetryPeriod}, nil
}

func (c *PodController) Register(ctx context.Context, m manager.Manager) error {
	b := controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: utilscontroller.LinearScaleReconciles(utilscontroller.CPUCount(ctx), minReconciles, maxReconciles)})
	if options.FromContext(ctx).FeatureGates.PodResourceHints {
		b = b.Watches(&v1.Pod{}, PodResourceHintsEventHandler(c.cluster))
	}
	return b.Complete(c)
}

// PodResourceHintsEventHandler marks the cluster unconsolidated when a pod's status.resourceHints change, or when a
// bound pod's effective requests change (e.g. an in-place resize), so consolidation re-evaluates. It enqueues nothing;
// the primary pod watch reconciles the pod.
func PodResourceHintsEventHandler(cluster *state.Cluster) handler.EventHandler {
	return handler.Funcs{
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			oldPod, ok := e.ObjectOld.(*v1.Pod)
			if !ok {
				return
			}
			newPod, ok := e.ObjectNew.(*v1.Pod)
			if !ok {
				return
			}
			if PodResourceHintsOrRequestsChanged(oldPod, newPod) {
				cluster.MarkUnconsolidated()
			}
		},
	}
}

// PodResourceHintsOrRequestsChanged reports whether consolidation should re-evaluate after a pod update.
func PodResourceHintsOrRequestsChanged(oldPod, newPod *v1.Pod) bool {
	if resources.ResourceHintsChanged(oldPod, newPod) {
		return true
	}
	if newPod.Spec.NodeName == "" {
		return false
	}
	return !equality.Semantic.DeepEqual(resources.Ceiling(oldPod).Requests, resources.Ceiling(newPod).Requests)
}
