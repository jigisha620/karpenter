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

package resources

import (
	"maps"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	resourcehelper "k8s.io/component-helpers/resource"
)

var podCount = resource.MustParse("1")

// HintedRequests returns the pod's requests with its replacement hint (what a replacement of the pod would request if
// it were created now) merged over its spec per container and per resource, taking the per-resource max across
// managers. Containers and resources without a hint keep their spec values, overhead is preserved, and podRequests
// apply only when pod-level resources are set. It returns false when the pod has no usable replacement hint, in which
// case callers keep today's behavior. Invalid entries (unknown container names, negative quantities) are ignored.
func HintedRequests(pod *v1.Pod) (v1.ResourceList, bool) {
	containers, podRequests, ok := mergeReplacementHints(pod)
	if !ok {
		return nil, false
	}
	spec := &v1.PodSpec{
		Containers:     applyContainerHints(pod.Spec.Containers, containers),
		InitContainers: applyContainerHints(pod.Spec.InitContainers, containers),
		Overhead:       pod.Spec.Overhead,
		Resources:      pod.Spec.Resources,
	}
	if len(podRequests) > 0 && resourcehelper.IsPodLevelResourcesSet(pod) {
		merged := make(v1.ResourceList, len(pod.Spec.Resources.Requests)+len(podRequests))
		maps.Copy(merged, pod.Spec.Resources.Requests)
		maps.Copy(merged, podRequests)
		spec.Resources = &v1.ResourceRequirements{Requests: merged, Limits: pod.Spec.Resources.Limits}
	}
	return RequestsForSpec(spec), true
}

// HintedPodRequests is HintedRequests plus a pod count of one, matching RequestsForPods for a single pod.
func HintedPodRequests(pod *v1.Pod) (v1.ResourceList, bool) {
	hinted, ok := HintedRequests(pod)
	if !ok {
		return nil, false
	}
	hinted[v1.ResourcePods] = podCount
	return hinted, true
}

// ResourceHintsChanged reports whether the pods' status.resourceHints differ.
func ResourceHintsChanged(oldPod, newPod *v1.Pod) bool {
	return !equality.Semantic.DeepEqual(oldPod.Status.ResourceHints, newPod.Status.ResourceHints)
}

func mergeReplacementHints(pod *v1.Pod) (map[string]v1.ResourceList, v1.ResourceList, bool) {
	if len(pod.Status.ResourceHints) == 0 {
		return nil, nil, false
	}
	names := map[string]struct{}{}
	for _, c := range pod.Spec.Containers {
		names[c.Name] = struct{}{}
	}
	for _, c := range pod.Spec.InitContainers {
		names[c.Name] = struct{}{}
	}
	containers := map[string]v1.ResourceList{}
	podRequests := v1.ResourceList{}
	found := false
	for i := range pod.Status.ResourceHints {
		values := pod.Status.ResourceHints[i].Replacement
		if values == nil {
			continue
		}
		for _, c := range values.Containers {
			if _, ok := names[c.Name]; !ok {
				continue
			}
			if containers[c.Name] == nil {
				containers[c.Name] = v1.ResourceList{}
			}
			if maxInto(containers[c.Name], c.Requests) {
				found = true
			}
		}
		if resourcehelper.IsPodLevelResourcesSet(pod) && maxInto(podRequests, values.PodRequests) {
			found = true
		}
	}
	return containers, podRequests, found
}

// maxInto sets dest to the per-resource max of dest and src, ignoring negative quantities, and reports whether any
// value from src was considered.
func maxInto(dest, src v1.ResourceList) bool {
	used := false
	for name, q := range src {
		if q.Sign() < 0 {
			continue
		}
		used = true
		if cur, ok := dest[name]; !ok || q.Cmp(cur) > 0 {
			dest[name] = q
		}
	}
	return used
}

func applyContainerHints(containers []v1.Container, hints map[string]v1.ResourceList) []v1.Container {
	result := make([]v1.Container, len(containers))
	for i, c := range containers {
		result[i] = c
		hinted, ok := hints[c.Name]
		if !ok || len(hinted) == 0 {
			continue
		}
		merged := make(v1.ResourceList, len(c.Resources.Requests)+len(hinted))
		maps.Copy(merged, c.Resources.Requests)
		maps.Copy(merged, hinted)
		result[i].Resources = v1.ResourceRequirements{Requests: merged, Limits: c.Resources.Limits}
	}
	return result
}
