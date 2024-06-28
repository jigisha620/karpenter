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

package readiness_test

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/karpenter/pkg/test/v1test1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/awslabs/operatorpkg/status"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	nodepoolreadiness "sigs.k8s.io/karpenter/pkg/controllers/nodepool/readiness"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var (
	nodePoolReadinessController *nodepoolreadiness.Controller
	ctx                         context.Context
	env                         *test.Environment
	cloudProvider               *fake.CloudProvider
	nodePool                    *v1beta1.NodePool
	nodeClass                   *v1test1.TestNodeClass
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Counter")
}

var _ = BeforeSuite(func() {
	cloudProvider = fake.NewCloudProvider()
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1test1.CRDs...))
	nodePoolReadinessController = nodepoolreadiness.NewController(env.Client, cloudProvider)
})
var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Counter", func() {
	BeforeEach(func() {
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		nodePool = test.NodePool()
		nodeClass = test.NodeClass(v1test1.TestNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name},
		})
	})
	AfterEach(func() { cloudProvider.NodeClassGroupVersionKind = nil })
	It("should have status condition on nodePool as not ready due to error when getting nodeClass", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		_ = ExpectObjectReconcileFailed(ctx, env.Client, nodePoolReadinessController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		Expect(nodePool.StatusConditions().IsTrue(status.ConditionReady)).To(BeFalse())
	})
	It("should have status condition on nodePool as ready if nodeClass is ready", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		_ = ExpectObjectReconciled(ctx, env.Client, nodePoolReadinessController, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		Expect(nodePool.StatusConditions().IsTrue(status.ConditionReady)).To(BeTrue())
	})
	It("should have status condition on nodePool as not ready if nodeClass is not ready", func() {
		nodeClass.Status = v1test1.TestNodeClassStatus{
			Conditions: []status.Condition{
				{
					Type:               status.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             "reason",
					Message:            "message",
					LastTransitionTime: metav1.Time{Time: time.Now()},
				},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		result := ExpectObjectReconciled(ctx, env.Client, nodePoolReadinessController, nodePool)
		Expect(result.Requeue).To(BeTrue())
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		Expect(nodePool.StatusConditions().IsTrue(status.ConditionReady)).To(BeFalse())
	})
	It("should have status condition on nodePool as not ready if nodeClass does not have status conditions", func() {
		nodeClass.Status = v1test1.TestNodeClassStatus{
			Conditions: []status.Condition{},
		}
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		result := ExpectObjectReconciled(ctx, env.Client, nodePoolReadinessController, nodePool)
		Expect(result.Requeue).To(BeTrue())
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		Expect(nodePool.StatusConditions().IsTrue(status.ConditionReady)).To(BeFalse())
	})
})
