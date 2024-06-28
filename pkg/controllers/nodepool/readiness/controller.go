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

package readiness

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/awslabs/operatorpkg/object"

	"github.com/awslabs/operatorpkg/status"
	"go.uber.org/multierr"

	"sigs.k8s.io/karpenter/pkg/utils/nodepool"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"knative.dev/pkg/logging"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// NewController is a constructor
func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("nodepool.status").With("nodepool", nodePool.Name))
	ctx = injection.WithControllerName(ctx, "nodepool.status")
	stored := nodePool.DeepCopy()
	supportedNC := c.cloudProvider.GetSupportedNodeClasses()
	var errs error
	var ready bool
	nodeClass, err := c.hasNodeClass(ctx, supportedNC, nodePool)
	if err != nil {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeNodeClassReady, "UnresolvedNodeClass", "Unable to resolve nodeClass")
		errs = multierr.Append(errs, err)
	} else {
		ready = c.setReadyCondition(nodePool, nodeClass)
	}
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		if err := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFrom(stored)); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	if errs != nil {
		return reconcile.Result{}, errs
	}
	return reconcile.Result{Requeue: !ready}, nil
}

func (c *Controller) setReadyCondition(nodePool *v1beta1.NodePool, nodeClass status.Object) bool {
	ready := nodeClass.StatusConditions().Get(status.ConditionReady)
	if ready.IsUnknown() {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeNodeClassReady, "NodeClassNotReady", "Node Class Not Ready")
		return false
	}
	if ready.IsFalse() {
		nodePool.StatusConditions().SetFalse(v1beta1.ConditionTypeNodeClassReady, ready.Reason, ready.Message)
		return false
	}
	nodePool.StatusConditions().SetTrue(v1beta1.ConditionTypeNodeClassReady)
	return true
}

func (c *Controller) hasNodeClass(ctx context.Context, supportedNC []status.Object, nodePool *v1beta1.NodePool) (status.Object, error) {
	if len(supportedNC) == 0 {
		return nil, fmt.Errorf("resolving nodeClass")
	}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, supportedNC[0]); err != nil {
		return nil, fmt.Errorf("resolving nodeClass, %w", err)
	}
	return supportedNC[0], nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	builder := controllerruntime.NewControllerManagedBy(m)
	for _, supportedNC := range c.cloudProvider.GetSupportedNodeClasses() {
		nodeclass := &unstructured.Unstructured{}
		ncGVK := object.GVK(supportedNC)
		nodeclass.SetGroupVersionKind(ncGVK)
		builder = builder.Watches(
			nodeclass,
			nodepool.NodeClassEventHandler(c.kubeClient),
		)
	}
	return builder.
		Named("nodepool.status").
		For(&v1beta1.NodePool{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
