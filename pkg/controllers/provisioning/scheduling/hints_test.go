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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Pod Resource Hints", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		original := ctx
		DeferCleanup(func() { ctx = original })
		nodePool = test.NodePool()
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType("small", fake.WithResources(corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("100"),
			})),
			fake.NewInstanceType("large", fake.WithResources(corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourcePods: resource.MustParse("100"),
			})),
		}
	})

	DescribeTable("should size pods on deleting nodes by their replacement hint only when the PodResourceHints gate is enabled",
		func(enabled bool, expectedInstanceType string) {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{PodResourceHints: lo.ToPtr(enabled)}}))
			ExpectApplied(ctx, env.Client, nodePool)
			pod := test.UnschedulablePod(test.PodOptions{ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[corev1.LabelInstanceTypeStable]).To(Equal("small"))

			ExpectResourceHints(ctx, env.Client, pod, corev1.PodResourceHint{
				Manager:     "example.com/vertical",
				UpdateTime:  metav1.NewTime(env.Clock.Now()),
				Replacement: test.ContainerHints(map[string]corev1.ResourceList{pod.Spec.Containers[0].Name: {corev1.ResourceCPU: resource.MustParse("4")}}),
			})
			cluster.MarkForDeletion(node.Spec.ProviderID)
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov)

			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodes).To(HaveLen(2))
			replacement, _ := lo.Find(nodes, func(n *corev1.Node) bool { return n.Name != node.Name })
			Expect(replacement.Labels[corev1.LabelInstanceTypeStable]).To(Equal(expectedInstanceType))
		},
		Entry("gate enabled: replacement 4 CPU needs the large instance type", true, "large"),
		Entry("gate disabled: current 1 CPU fits the small instance type", false, "small"),
	)
})
