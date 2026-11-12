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

package state_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Pod Resource Hints", func() {
	var node *corev1.Node
	BeforeEach(func() {
		node = test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			ProviderID:  test.RandomProviderID(),
		})
	})

	DescribeTable("should count a running pod at its requests regardless of its replacement hint",
		func(enabled bool) {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{PodResourceHints: lo.ToPtr(enabled)}}))
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
			})
			pod.Status.ResourceHints = []corev1.PodResourceHint{{
				Manager:     "example.com/vertical",
				UpdateTime:  metav1.NewTime(env.Clock.Now()),
				Replacement: test.ContainerHints(map[string]corev1.ResourceList{pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("3")}}),
			}}
			ExpectApplied(ctx, env.Client, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))

			// C3: hints don't change a node's utilization or headroom
			ExpectResources(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}, ExpectStateNodeExists(cluster, node).PodRequests())
		},
		Entry("gate enabled", true),
		Entry("gate disabled", false),
	)
	It("should refresh the cached DaemonSet pod when that pod's hints change", func() {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{PodResourceHints: lo.ToPtr(true)}}))
		ds := test.DaemonSet(test.DaemonSetOptions{PodOptions: test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
		}})
		ExpectApplied(ctx, env.Client, ds)
		dsPod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}}},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
		})
		ExpectApplied(ctx, env.Client, node, dsPod)
		ExpectManualBinding(ctx, env.Client, dsPod, node)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(ds))
		Expect(cluster.GetDaemonSetPod(ds).Status.ResourceHints).To(BeEmpty())

		hint := corev1.PodResourceHint{
			Manager:     "example.com/vertical",
			UpdateTime:  metav1.NewTime(env.Clock.Now()),
			Replacement: test.ContainerHints(map[string]corev1.ResourceList{dsPod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("1")}}),
		}
		ExpectResourceHints(ctx, env.Client, dsPod, hint)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(dsPod))
		Expect(cluster.GetDaemonSetPod(ds).Status.ResourceHints).To(HaveLen(1))
	})
	Context("Re-evaluation", func() {
		var pod *corev1.Pod
		BeforeEach(func() {
			pod = test.Pod(test.PodOptions{
				NodeName:             "node",
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
			})
		})
		update := func(oldPod, newPod *corev1.Pod) {
			informer.PodResourceHintsEventHandler(cluster).Update(ctx, event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}, nil)
		}
		It("should mark the cluster unconsolidated when a pod's hints change", func() {
			updated := pod.DeepCopy()
			updated.Status.ResourceHints = []corev1.PodResourceHint{{
				Manager:     "example.com/vertical",
				Replacement: test.ContainerHints(map[string]corev1.ResourceList{pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("3")}}),
			}}
			state := cluster.ConsolidationState()
			env.Clock.Step(time.Second)
			update(pod, updated)
			Expect(cluster.ConsolidationState()).ToNot(Equal(state))
		})
		It("should mark the cluster unconsolidated when an in-place resize changes a bound pod's requests", func() {
			updated := pod.DeepCopy()
			updated.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
			state := cluster.ConsolidationState()
			env.Clock.Step(time.Second)
			update(pod, updated)
			Expect(cluster.ConsolidationState()).ToNot(Equal(state))
		})
		It("should not mark the cluster unconsolidated when hints and requests are unchanged", func() {
			updated := pod.DeepCopy()
			updated.Labels = map[string]string{"changed": "true"}
			state := cluster.ConsolidationState()
			env.Clock.Step(time.Second)
			update(pod, updated)
			Expect(cluster.ConsolidationState()).To(Equal(state))
		})
	})
})
