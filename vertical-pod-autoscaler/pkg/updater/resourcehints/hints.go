/*
Copyright 2026 The Kubernetes Authors.

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

// Package resourcehints computes and publishes Pod status.resourceHints
// entries for pods controlled by a VPA.
package resourcehints

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/admission-controller/resource/pod/patch"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	resourcehelpers "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/resources"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
)

// ManagerName is the manager and field manager of the updater's hint entry.
const ManagerName = "autoscaling.k8s.io/vpa-updater"

// Reason is the updater decision that triggered a hint computation.
type Reason string

const (
	// ReasonNone means no actuation decision was made for the pod this loop.
	ReasonNone Reason = ""
	// ReasonResized means the updater issued an in-place resize.
	ReasonResized Reason = "Resized"
)

// Calculator computes the updater's hint entry for a pod.
type Calculator struct {
	replacement patch.ReplacementCalculator
	now         func() time.Time
}

// NewCalculator returns a Calculator. replacement must use the same logic
// and flags as the admission controller.
func NewCalculator(replacement patch.ReplacementCalculator) *Calculator {
	return &Calculator{
		replacement: replacement,
		now:         time.Now,
	}
}

// Compute returns the desired hint entry, or nil when replacement equals the
// pod's current requests (W2).
func (c *Calculator) Compute(pod *corev1.Pod, vpa *vpa_types.VerticalPodAutoscaler) (*corev1.PodResourceHint, error) {
	pod = pod.DeepCopy()
	resources, err := c.replacement.CalculateReplacementResources(pod, vpa)
	if err != nil {
		return nil, err
	}
	replacement := map[string]corev1.ResourceList{}
	for i := range resources {
		if i >= len(pod.Spec.Containers) {
			break
		}
		replacement[pod.Spec.Containers[i].Name] = resources[i].Requests
	}
	values := replacementValues(pod, vpa, replacement, currentRequests(pod))
	if values == nil {
		return nil, nil
	}
	return &corev1.PodResourceHint{
		Manager:     ManagerName,
		UpdateTime:  metav1.NewTime(c.now()),
		Replacement: values,
	}, nil
}

// currentRequests returns each container's effective requests, from
// status.containerStatuses[].resources when present, else spec.
func currentRequests(pod *corev1.Pod) map[string]corev1.ResourceList {
	out := map[string]corev1.ResourceList{}
	for _, c := range pod.Spec.Containers {
		req, _ := resourcehelpers.ContainerRequestsAndLimits(c.Name, pod)
		out[c.Name] = req
	}
	return out
}

// replacementValues builds ResourceHintValues for controlled containers and
// resources. Resources equal to the current request are dropped (W2).
func replacementValues(pod *corev1.Pod, vpa *vpa_types.VerticalPodAutoscaler, want, current map[string]corev1.ResourceList) *corev1.ResourceHintValues {
	var out corev1.ResourceHintValues
	for _, c := range pod.Spec.Containers {
		w := want[c.Name]
		if len(w) == 0 {
			continue
		}
		controlled := controlledResources(c.Name, vpa)
		if len(controlled) == 0 {
			continue
		}
		reqs := corev1.ResourceList{}
		for _, rn := range controlled {
			q, ok := w[rn]
			if !ok {
				continue
			}
			if cur, ok := current[c.Name][rn]; ok && cur.Cmp(q) == 0 {
				continue
			}
			reqs[rn] = q.DeepCopy()
		}
		if len(reqs) > 0 {
			out.Containers = append(out.Containers, corev1.ContainerResourceHint{Name: c.Name, Requests: reqs})
		}
	}
	if len(out.Containers) == 0 {
		return nil
	}
	return &out
}

func controlledResources(container string, vpa *vpa_types.VerticalPodAutoscaler) []corev1.ResourceName {
	policy := vpa_api_util.GetContainerResourcePolicy(container, vpa.Spec.ResourcePolicy)
	if policy != nil && policy.Mode != nil && *policy.Mode == vpa_types.ContainerScalingModeOff {
		return nil
	}
	if policy != nil && policy.ControlledResources != nil {
		return *policy.ControlledResources
	}
	return []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory}
}

// ExistingHint returns the updater's current entry on the pod, if any.
func ExistingHint(pod *corev1.Pod) *corev1.PodResourceHint {
	for i := range pod.Status.ResourceHints {
		if pod.Status.ResourceHints[i].Manager == ManagerName {
			return &pod.Status.ResourceHints[i]
		}
	}
	return nil
}

// NeedsWrite implements W3: rewrite after an in-place resize, when a
// replacement quantity increased by any amount, when one decreased by more
// than threshold (relative), or when a container/resource appeared or
// disappeared. Increases are written promptly because a hint that is too
// small can still cause churn; one that is too large is only conservative.
func NeedsWrite(existing, desired *corev1.PodResourceHint, threshold float64, reason Reason) bool {
	if existing == nil && desired == nil {
		return false
	}
	if existing == nil || desired == nil {
		return true
	}
	if reason == ReasonResized {
		return true
	}
	return replacementChanged(existing.Replacement, desired.Replacement, threshold)
}

func replacementChanged(existing, desired *corev1.ResourceHintValues, threshold float64) bool {
	em, dm := flatten(existing), flatten(desired)
	if len(em) != len(dm) {
		return true
	}
	for k, eq := range em {
		dq, ok := dm[k]
		if !ok {
			return true
		}
		if dq.Cmp(eq) > 0 {
			return true
		}
		if relDiff(eq, dq) > threshold {
			return true
		}
	}
	return false
}

type key struct {
	container string
	resource  corev1.ResourceName
}

func flatten(v *corev1.ResourceHintValues) map[key]resource.Quantity {
	out := map[key]resource.Quantity{}
	if v == nil {
		return out
	}
	for _, c := range v.Containers {
		for rn, q := range c.Requests {
			out[key{c.Name, rn}] = q
		}
	}
	return out
}

func relDiff(a, b resource.Quantity) float64 {
	av, bv := float64(a.MilliValue()), float64(b.MilliValue())
	if av == bv {
		return 0
	}
	if av == 0 {
		return 1
	}
	d := (bv - av) / av
	if d < 0 {
		d = -d
	}
	return d
}
