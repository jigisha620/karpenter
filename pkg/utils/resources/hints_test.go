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

package resources_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var _ = Describe("Resource Hints", func() {
	var pod *v1.Pod
	var app string
	BeforeEach(func() {
		pod = test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")},
			},
		})
		app = pod.Spec.Containers[0].Name
	})

	It("should report no hint when the pod has none", func() {
		_, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeFalse())
	})
	It("should merge a replacement per container and per resource over the spec", func() {
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager:     "example.com/a",
			Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("3")}}),
		}}
		hinted, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeTrue())
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("3"), v1.ResourceMemory: resource.MustParse("1Gi")})
	})
	It("should allow a replacement smaller than current requests", func() {
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager:     "example.com/a",
			Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("250m")}}),
		}}
		hinted, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeTrue())
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("1Gi")})
	})
	It("should take the per-resource max across managers", func() {
		pod.Status.ResourceHints = []v1.PodResourceHint{
			{Manager: "example.com/a", Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("512Mi")}})},
			{Manager: "example.com/b", Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("1500m"), v1.ResourceMemory: resource.MustParse("3Gi")}})},
		}
		hinted, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeTrue())
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("3Gi")})
	})
	It("should apply hints to init containers and native sidecars", func() {
		pod = test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")}},
			InitContainers: []v1.Container{
				{RestartPolicy: lo.ToPtr(v1.ContainerRestartPolicyAlways), Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")}}},
				{Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")}}},
			},
		})
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager: "example.com/a",
			Replacement: test.ContainerHints(map[string]v1.ResourceList{
				pod.Spec.InitContainers[0].Name: {v1.ResourceCPU: resource.MustParse("2")},
				pod.Spec.InitContainers[1].Name: {v1.ResourceCPU: resource.MustParse("4")},
			}),
		}}
		hinted, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeTrue())
		// init runs beside the sidecar: max(2 + 4, 2 + 1) = 6
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("6")})
	})
	It("should apply podRequests only when pod-level resources are set", func() {
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager:     "example.com/a",
			Replacement: &v1.ResourceHintValues{PodRequests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("5")}},
		}}
		_, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeFalse())

		pod.Spec.Resources = &v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("2Gi")}}
		hinted, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeTrue())
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("5"), v1.ResourceMemory: resource.MustParse("2Gi")})
	})
	It("should preserve pod overhead", func() {
		pod.Spec.Overhead = v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")}
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager:     "example.com/a",
			Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("2")}}),
		}}
		hinted, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeTrue())
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("2100m"), v1.ResourceMemory: resource.MustParse("1Gi")})
	})
	It("should fail open on unknown containers and negative quantities", func() {
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager: "example.com/a",
			Replacement: test.ContainerHints(map[string]v1.ResourceList{
				"missing": {v1.ResourceCPU: resource.MustParse("8")},
				app:       {v1.ResourceCPU: resource.MustParse("-1")},
			}),
		}}
		_, ok := resources.HintedRequests(pod)
		Expect(ok).To(BeFalse())
		Expect(resources.RequestsForPods(pod)).To(Equal(resources.RequestsForPods(pod.DeepCopy())))
	})
	It("should add a pod count in HintedPodRequests", func() {
		pod.Status.ResourceHints = []v1.PodResourceHint{{
			Manager:     "example.com/a",
			Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("2")}}),
		}}
		hinted, ok := resources.HintedPodRequests(pod)
		Expect(ok).To(BeTrue())
		ExpectResources(hinted, v1.ResourceList{v1.ResourceCPU: resource.MustParse("2"), v1.ResourceMemory: resource.MustParse("1Gi"), v1.ResourcePods: resource.MustParse("1")})
	})
	It("should detect hint changes", func() {
		updated := pod.DeepCopy()
		Expect(resources.ResourceHintsChanged(pod, updated)).To(BeFalse())
		updated.Status.ResourceHints = []v1.PodResourceHint{{Manager: "example.com/a", Replacement: test.ContainerHints(map[string]v1.ResourceList{app: {v1.ResourceCPU: resource.MustParse("2")}})}}
		Expect(resources.ResourceHintsChanged(pod, updated)).To(BeTrue())
	})
})
