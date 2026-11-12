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

package disruption_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Pod Resource Hints", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node

	withGate := func(enabled bool) {
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{PodResourceHints: lo.ToPtr(enabled)}}))
	}
	ownedPod := func(cpu string) *corev1.Pod {
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
		return test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "test"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		})
	}
	replacement := func(pod *corev1.Pod, cpu string) corev1.PodResourceHint {
		return corev1.PodResourceHint{
			Manager:     "example.com/vertical",
			UpdateTime:  metav1.NewTime(env.Clock.Now()),
			Replacement: test.ContainerHints(map[string]corev1.ResourceList{pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse(cpu)}}),
		}
	}
	reconcileDisruption := func() {
		dc := disruption.NewController(ctx, env.Clock, env.Client, prov, cloudProvider, recorder, cluster, queue, clusterCost, disruption.WithMethods(NewMethodsWithNopValidator()...))
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		ExpectSingletonReconciled(ctx, dc)
	}
	expectConsolidatedToCheap := func(expected bool) {
		if !expected {
			Expect(queue.GetCommands()).To(BeEmpty())
			return
		}
		cmds := queue.GetCommands()
		Expect(cmds).To(HaveLen(1))
		Expect(cmds[0].Replacements).To(HaveLen(1))
		Expect(cmds[0].Replacements[0].InstanceTypeOptions).To(ContainElement(HaveField("Name", "cheap")))
	}

	BeforeEach(func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType("expensive",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				}),
				fake.WithOfferings(cloudprovider.Offering{
					Available:    true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        1.0,
				}),
			),
			fake.NewInstanceType("cheap",
				fake.WithResources(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				}),
				fake.WithOfferings(cloudprovider.Offering{
					Available:    true,
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1a"}),
					Price:        0.5,
				}),
			),
		}
		ExpectSingletonReconciled(ctx, pricingController)

		nodePool = test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Disruption: v1.Disruption{
					ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
					Budgets:             []v1.Budget{{Nodes: "100%"}},
					ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				},
			},
		})
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: "expensive",
					v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone:       "test-zone-1a",
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
					corev1.ResourcePods:   resource.MustParse("100"),
				},
			},
		})
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeConsolidatable)
	})

	DescribeTable("should size a moved pod by its replacement hint only when the PodResourceHints gate is enabled",
		func(enabled bool, expectConsolidation bool) {
			withGate(enabled)
			pod := ownedPod("1")
			pod.Status.ResourceHints = []corev1.PodResourceHint{replacement(pod, "3")}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			Expect(pod.Status.ResourceHints).To(HaveLen(1))

			reconcileDisruption()
			expectConsolidatedToCheap(expectConsolidation)
		},
		Entry("gate enabled: replacement 3 CPU doesn't fit on the cheap node", true, false),
		Entry("gate disabled: replacement ignored, current 1 CPU fits on the cheap node", false, true),
	)
	DescribeTable("should consolidate when the replacement hint is smaller than current requests",
		func(enabled bool, expectConsolidation bool) {
			withGate(enabled)
			pod := ownedPod("3")
			pod.Status.ResourceHints = []corev1.PodResourceHint{replacement(pod, "1")}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)

			reconcileDisruption()
			expectConsolidatedToCheap(expectConsolidation)
		},
		Entry("gate enabled: replacement 1 CPU fits on the cheap node", true, true),
		Entry("gate disabled: current 3 CPU doesn't fit on the cheap node", false, false),
	)
	It("should take the max replacement across managers", func() {
		withGate(true)
		pod := ownedPod("1")
		small := replacement(pod, "500m")
		small.Manager = "example.com/small"
		pod.Status.ResourceHints = []corev1.PodResourceHint{small, replacement(pod, "3")}
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		reconcileDisruption()
		expectConsolidatedToCheap(false)
	})
	DescribeTable("should size new-node DaemonSet overhead by the DaemonSet pod's replacement hint",
		func(enabled bool, expectConsolidation bool) {
			withGate(enabled)
			// workload 1 CPU + DaemonSet replacement 1500m = 2500m doesn't fit the 2 CPU cheap node; 1 CPU + 100m does
			ds := test.DaemonSet(test.DaemonSetOptions{PodOptions: test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			}})
			ExpectApplied(ctx, env.Client, ds)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())
			dsPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID,
					Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
				}}},
				ResourceRequirements: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			})
			dsPod.Status.ResourceHints = []corev1.PodResourceHint{replacement(dsPod, "1500m")}
			workload := ownedPod("1")

			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, dsPod, workload)
			ExpectManualBinding(ctx, env.Client, dsPod, node)
			ExpectManualBinding(ctx, env.Client, workload, node)
			ExpectReconcileSucceeded(ctx, informer.NewDaemonSetController(env.Client, cluster), client.ObjectKeyFromObject(ds))

			reconcileDisruption()
			expectConsolidatedToCheap(expectConsolidation)
		},
		Entry("gate enabled: DaemonSet replacement 1500m blocks the cheap node", true, false),
		Entry("gate disabled: DaemonSet current 100m fits on the cheap node", false, true),
	)
})
