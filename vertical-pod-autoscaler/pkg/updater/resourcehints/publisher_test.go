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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/features"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/test"
)

func recordingClient() (*fake.Clientset, *[]core.PatchActionImpl) {
	client := fake.NewClientset()
	var patches []core.PatchActionImpl
	client.PrependReactor("patch", "pods", func(action core.Action) (bool, runtime.Object, error) {
		patches = append(patches, action.(core.PatchActionImpl))
		return true, &corev1.Pod{}, nil
	})
	return client, &patches
}

type applyBody struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Status map[string]json.RawMessage `json:"status"`
}

func TestPublishAppliesToResourceHintsSubresource(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.MutableFeatureGate, features.CPUStartupBoost, true)
	factor6 := int32(6)
	vpa := test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
		WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
		WithTarget("500m", "1Gi").
		WithCPUStartupBoost(vpa_types.FactorStartupBoostType, &factor6, nil, 90).Get()
	pod := podWith("1", "1Gi")

	client, patches := recordingClient()
	p := NewPublisher(client, newTestCalculator(), 0.1)
	require.NoError(t, p.Publish(context.Background(), pod, vpa, ReasonNone))
	require.Len(t, *patches, 1)

	a := (*patches)[0]
	assert.Equal(t, "resourcehints", a.GetSubresource())
	assert.Equal(t, "default", a.GetNamespace())
	assert.Equal(t, "pod", a.GetName())
	assert.Equal(t, types.ApplyPatchType, a.GetPatchType())
	assert.Equal(t, ManagerName, a.PatchOptions.FieldManager)
	require.NotNil(t, a.PatchOptions.Force)
	assert.True(t, *a.PatchOptions.Force)

	var body applyBody
	require.NoError(t, json.Unmarshal(a.GetPatch(), &body))
	assert.Equal(t, "v1", body.APIVersion)
	assert.Equal(t, "Pod", body.Kind)
	assert.Equal(t, "pod", body.Metadata.Name)
	assert.Equal(t, "default", body.Metadata.Namespace)
	assert.Len(t, body.Status, 1, "only resourceHints may be set: %s", a.GetPatch())

	var hints []corev1.PodResourceHint
	require.NoError(t, json.Unmarshal(body.Status["resourceHints"], &hints))
	require.Len(t, hints, 1)
	h := hints[0]
	assert.Equal(t, ManagerName, h.Manager)
	assert.False(t, h.UpdateTime.IsZero())
	require.NotNil(t, h.Replacement)
	require.Len(t, h.Replacement.Containers, 1)
	assert.Equal(t, containerName, h.Replacement.Containers[0].Name)
	cpu := h.Replacement.Containers[0].Requests[corev1.ResourceCPU]
	assert.Equal(t, "3", cpu.String())
	_, hasMem := h.Replacement.Containers[0].Requests[corev1.ResourceMemory]
	assert.False(t, hasMem, "memory equals current request and must be omitted")
}

func TestPublishSkipsUnchangedEntry(t *testing.T) {
	vpa := test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
		WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
		WithTarget("2", "1Gi").Get()
	pod := podWith("1", "1Gi")
	desired, err := newTestCalculator().Compute(pod, vpa)
	require.NoError(t, err)
	require.NotNil(t, desired)
	pod.Status.ResourceHints = []corev1.PodResourceHint{*desired}

	client, patches := recordingClient()
	p := NewPublisher(client, newTestCalculator(), 0.1)
	require.NoError(t, p.Publish(context.Background(), pod, vpa, ReasonNone))
	assert.Empty(t, *patches)

	require.NoError(t, p.Publish(context.Background(), pod, vpa, ReasonResized))
	assert.Len(t, *patches, 1, "a resize decision forces a rewrite")
}

func TestPublishRemovesEntryWhenStable(t *testing.T) {
	vpa := test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
		WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
		WithTarget("1", "1Gi").Get()
	pod := podWith("1", "1Gi")
	pod.Status.ResourceHints = []corev1.PodResourceHint{{Manager: ManagerName, Replacement: vals(rl("cpu", "2"))}}

	client, patches := recordingClient()
	p := NewPublisher(client, newTestCalculator(), 0.1)
	require.NoError(t, p.Publish(context.Background(), pod, vpa, ReasonNone))
	require.Len(t, *patches, 1)
	assertEmptyStatus(t, (*patches)[0])
}

func TestRemove(t *testing.T) {
	client, patches := recordingClient()
	p := NewPublisher(client, newTestCalculator(), 0.1)

	pod := podWith("1", "1Gi")
	pod.Status.ResourceHints = []corev1.PodResourceHint{{Manager: "example.com/psi-agent"}}
	require.NoError(t, p.Remove(context.Background(), pod))
	assert.Empty(t, *patches, "no entry of ours, nothing to remove")

	pod.Status.ResourceHints = append(pod.Status.ResourceHints, corev1.PodResourceHint{Manager: ManagerName})
	require.NoError(t, p.Remove(context.Background(), pod))
	require.Len(t, *patches, 1)
	assertEmptyStatus(t, (*patches)[0])
}

func assertEmptyStatus(t *testing.T, a core.PatchActionImpl) {
	t.Helper()
	assert.Equal(t, "resourcehints", a.GetSubresource())
	assert.Equal(t, types.ApplyPatchType, a.GetPatchType())
	assert.Equal(t, ManagerName, a.PatchOptions.FieldManager)
	var body applyBody
	require.NoError(t, json.Unmarshal(a.GetPatch(), &body))
	assert.Empty(t, body.Status, "apply without our entry drops it: %s", a.GetPatch())
}
