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

package resourcehints

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
)

// Subresource is the pod subresource that carries status.resourceHints.
const Subresource = "resourcehints"

// Publisher writes the updater's hint entry with server-side apply.
type Publisher struct {
	client     kube_client.Interface
	calculator *Calculator
	threshold  float64
}

// NewPublisher returns a Publisher. threshold is the relative change below
// which an existing entry is left alone (W3).
func NewPublisher(client kube_client.Interface, calculator *Calculator, threshold float64) *Publisher {
	return &Publisher{client: client, calculator: calculator, threshold: threshold}
}

// Publish computes the pod's hint and applies it if W3 allows. A nil desired
// hint removes an existing entry.
func (p *Publisher) Publish(ctx context.Context, pod *corev1.Pod, vpa *vpa_types.VerticalPodAutoscaler, reason Reason) error {
	desired, err := p.calculator.Compute(pod, vpa)
	if err != nil {
		return err
	}
	existing := ExistingHint(pod)
	if !NeedsWrite(existing, desired, p.threshold, reason) {
		return nil
	}
	klog.V(4).InfoS("Publishing resource hint", "pod", klog.KObj(pod), "reason", reason, "remove", desired == nil)
	return p.apply(ctx, pod, desired)
}

// Remove drops the updater's entry from a pod it no longer manages (W4).
func (p *Publisher) Remove(ctx context.Context, pod *corev1.Pod) error {
	if ExistingHint(pod) == nil {
		return nil
	}
	klog.V(4).InfoS("Removing resource hint", "pod", klog.KObj(pod))
	return p.apply(ctx, pod, nil)
}

func (p *Publisher) apply(ctx context.Context, pod *corev1.Pod, hint *corev1.PodResourceHint) error {
	body, err := ApplyBody(pod, hint)
	if err != nil {
		return err
	}
	_, err = p.client.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: ManagerName, Force: ptr.To(true)}, Subresource)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("applying resource hints to pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// ApplyBody builds the server-side apply body. A nil hint yields an empty
// status, which removes the entry this manager owns.
func ApplyBody(pod *corev1.Pod, hint *corev1.PodResourceHint) ([]byte, error) {
	status := corev1ac.PodStatus()
	if hint != nil {
		h := corev1ac.PodResourceHint().
			WithManager(hint.Manager).
			WithUpdateTime(hint.UpdateTime)
		if hint.Replacement != nil {
			h.WithReplacement(toApply(hint.Replacement))
		}
		status.WithResourceHints(h)
	}
	return json.Marshal(corev1ac.Pod(pod.Name, pod.Namespace).WithStatus(status))
}

func toApply(v *corev1.ResourceHintValues) *corev1ac.ResourceHintValuesApplyConfiguration {
	out := corev1ac.ResourceHintValues()
	for _, c := range v.Containers {
		out.WithContainers(corev1ac.ContainerResourceHint().WithName(c.Name).WithRequests(c.Requests))
	}
	if len(v.PodRequests) > 0 {
		out.WithPodRequests(v.PodRequests)
	}
	return out
}
