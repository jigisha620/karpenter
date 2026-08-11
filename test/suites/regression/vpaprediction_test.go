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

package integration_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

var _ = Describe("VPAPrediction", func() {
	BeforeEach(func() {
		if !env.VPACRDExists() {
			Skip("VPA CRD not installed, skipping VPA prediction tests")
		}

		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("120s")
	})
	AfterEach(func() {
		env.CleanupVPAs()
	})
	It("should size drift replacement using VPA predictions rather than current requests", func() {
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 1,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: testLabels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		})

		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(labelSelector, 1)
		originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 1)
		env.EventuallyExpectNodeClaimsReady(originalNodeClaims...)

		vpa := common.VPA("vpa-drift-test", dep.Name, dep.Namespace)
		env.ExpectCreated(vpa)
		env.UpdateVPAStatus(vpa, dep.Spec.Template.Spec.Containers[0].Name, corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		})

		// Trigger drift by changing the NodePool template annotation
		nodePool.Spec.Template.Annotations = map[string]string{"test-annotation": "drift"}
		env.ExpectUpdated(nodePool)

		// Replacement should be sized using prediction (500m) not current requests (3 CPU)
		expectReplacementSizedWithPrediction(originalNodeClaims[0], 3000, 500)
	})
	It("should use VPA predictions for both workload and daemonset when sizing replacement", func() {
		ds := test.DaemonSet(test.DaemonSetOptions{
			PodOptions: test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
			},
		})

		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 1,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: testLabels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		})

		env.ExpectCreated(nodeClass, nodePool, ds, dep)
		env.EventuallyExpectHealthyPodCount(labelSelector, 1)
		originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 1)
		env.EventuallyExpectNodeClaimsReady(originalNodeClaims...)

		vpaWorkload := common.VPA("vpa-workload", dep.Name, dep.Namespace)
		env.ExpectCreated(vpaWorkload)
		env.UpdateVPAStatus(vpaWorkload, dep.Spec.Template.Spec.Containers[0].Name, corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		})

		// Create VPA for the daemonset predicting 1 CPU (10x current 100m)
		vpaDaemon := common.VPAForDaemonSet("vpa-daemon", ds.Name, ds.Namespace)
		env.ExpectCreated(vpaDaemon)
		env.UpdateVPAStatus(vpaDaemon, ds.Spec.Template.Spec.Containers[0].Name, corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"),
		})

		// Replacement should account for predicted workload (500m) + predicted daemon (1 CPU)
		// Total pod requests on original: 3000 + 100 = 3100m
		// Total predicted: 500 + 1000 = 1500m
		expectReplacementSizedWithPrediction(originalNodeClaims[0], 3100, 1500)
	})
	It("should include pod overhead in VPA prediction when sizing replacement", func() {
		rc := &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "test-overhead"},
			Handler:    "runc",
			Overhead: &nodev1.Overhead{
				PodFixed: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
		}
		env.ExpectCreated(rc)
		DeferCleanup(func() { env.ExpectDeleted(rc) })

		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 1,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: testLabels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		})
		dep.Spec.Template.Spec.RuntimeClassName = lo.ToPtr("test-overhead")

		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(labelSelector, 1)
		originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 1)
		env.EventuallyExpectNodeClaimsReady(originalNodeClaims...)

		vpa := common.VPA("vpa-overhead-test", dep.Name, dep.Namespace)
		env.ExpectCreated(vpa)
		env.UpdateVPAStatus(vpa, dep.Spec.Template.Spec.Containers[0].Name, corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		})

		nodePool.Spec.Template.Annotations = map[string]string{"test-annotation": "drift"}
		env.ExpectUpdated(nodePool)

		// Replacement should be sized using prediction (500m) + overhead (200m) = 700m
		// Original pod requests were 3000m + 200m overhead = 3200m total
		expectReplacementSizedWithPrediction(originalNodeClaims[0], 3200, 700)
	})
	It("should include startup boost in VPA prediction when sizing replacement", func() {
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 1,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: testLabels},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			},
		})

		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(labelSelector, 1)
		originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 1)
		env.EventuallyExpectNodeClaimsReady(originalNodeClaims...)

		vpa := common.VPAWithStartupBoost("vpa-boost-test", dep.Name, dep.Namespace, 3)
		env.ExpectCreated(vpa)
		env.UpdateVPAStatus(vpa, dep.Spec.Template.Spec.Containers[0].Name, corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		})

		nodePool.Spec.Template.Annotations = map[string]string{"test-annotation": "drift"}
		env.ExpectUpdated(nodePool)

		// Replacement should be sized using boosted prediction (500m * 3 = 1500m) not target (500m)
		expectReplacementSizedWithPrediction(originalNodeClaims[0], 3000, 1500)
	})

})

func expectReplacementSizedWithPrediction(original *v1.NodeClaim, podRequestMillis, predictionMillis int64) {
	Eventually(func(g Gomega) {
		nodeClaims := &v1.NodeClaimList{}
		g.Expect(env.Client.List(env, nodeClaims, client.HasLabels{test.DiscoveryLabel})).To(Succeed())
		g.Expect(nodeClaims.Items).To(HaveLen(1))
		g.Expect(nodeClaims.Items[0].Name).NotTo(Equal(original.Name))
		g.Expect(nodeClaims.Items[0].StatusConditions().IsTrue(v1.ConditionTypeInitialized)).To(BeTrue())
		// Replacement should have (prediction + system overhead) where system overhead = original - podRequests.
		expectedCPU := predictionMillis + original.Spec.Resources.Requests.Cpu().MilliValue() - podRequestMillis
		g.Expect(nodeClaims.Items[0].Spec.Resources.Requests.Cpu().MilliValue()).To(Equal(expectedCPU))
	}).WithTimeout(5 * time.Minute).Should(Succeed())
}
