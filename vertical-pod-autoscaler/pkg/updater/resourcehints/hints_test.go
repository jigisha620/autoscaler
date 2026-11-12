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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/admission-controller/resource/pod/patch"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/admission-controller/resource/pod/recommendation"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/features"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/limitrange"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/test"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
)

const containerName = "app"

var fixedNow = time.Date(2026, 10, 16, 18, 2, 11, 0, time.UTC)

func newTestCalculator() *Calculator {
	lr := limitrange.NewNoopLimitsCalculator()
	processor := vpa_api_util.NewCappingRecommendationProcessor(lr)
	provider := recommendation.NewProvider(lr, processor)
	c := NewCalculator(patch.NewReplacementCalculator(provider, resource.QuantityValue{}))
	c.now = func() time.Time { return fixedNow }
	return c
}

func podWith(cpu, mem string) *corev1.Pod {
	return test.Pod().WithName("pod").
		AddContainer(test.Container().WithName(containerName).
			WithCPURequest(resource.MustParse(cpu)).
			WithMemRequest(resource.MustParse(mem)).Get()).
		Get()
}

func vals(reqs corev1.ResourceList) *corev1.ResourceHintValues {
	return &corev1.ResourceHintValues{Containers: []corev1.ContainerResourceHint{{Name: containerName, Requests: reqs}}}
}

func rl(kv ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		out[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return out
}

func TestCompute(t *testing.T) {
	factor6 := int32(6)
	resizedPod := podWith("2", "1Gi")
	resizedPod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:      containerName,
		Resources: &corev1.ResourceRequirements{Requests: rl("cpu", "500m", "memory", "1Gi")},
	}}

	tests := []struct {
		name  string
		pod   *corev1.Pod
		vpa   *vpa_types.VerticalPodAutoscaler
		boost bool
		want  *corev1.PodResourceHint
	}{
		{
			name: "startup boost: replacement is boosted cpu",
			pod:  podWith("1", "1Gi"),
			vpa: test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
				WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
				WithTarget("500m", "1Gi").
				WithCPUStartupBoost(vpa_types.FactorStartupBoostType, &factor6, nil, 90).Get(),
			boost: true,
			want: &corev1.PodResourceHint{
				Manager:     ManagerName,
				UpdateTime:  metav1.NewTime(fixedNow),
				Replacement: vals(rl("cpu", "3")),
			},
		},
		{
			name: "replacement equal to current requests: no hint",
			pod:  podWith("500m", "1Gi"),
			vpa: test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
				WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
				WithTarget("500m", "1Gi").Get(),
			want: nil,
		},
		{
			name: "only changed resource is listed",
			pod:  podWith("500m", "1Gi"),
			vpa: test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
				WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
				WithTarget("500m", "2Gi").Get(),
			want: &corev1.PodResourceHint{
				Manager:     ManagerName,
				UpdateTime:  metav1.NewTime(fixedNow),
				Replacement: vals(rl("memory", "2Gi")),
			},
		},
		{
			name: "replacement compared to allocated, not spec",
			pod:  resizedPod,
			vpa: test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
				WithUpdateMode(vpa_types.UpdateModeInPlace).
				WithTarget("2", "1Gi").Get(),
			want: &corev1.PodResourceHint{
				Manager:     ManagerName,
				UpdateTime:  metav1.NewTime(fixedNow),
				Replacement: vals(rl("cpu", "2")),
			},
		},
		{
			name: "scaling mode off: no hint",
			pod:  podWith("1", "1Gi"),
			vpa: test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
				WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
				WithTarget("2", "2Gi").WithScalingMode(containerName, vpa_types.ContainerScalingModeOff).Get(),
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.MutableFeatureGate, features.CPUStartupBoost, tc.boost)
			got, err := newTestCalculator().Compute(tc.pod, tc.vpa)
			require.NoError(t, err)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assertHintEqual(t, tc.want, got)
		})
	}
}

func TestComputeControlledResourcesOnly(t *testing.T) {
	vpa := test.VerticalPodAutoscaler().WithName("vpa").WithContainer(containerName).
		WithUpdateMode(vpa_types.UpdateModeInPlaceOrRecreate).
		WithTarget("2", "2Gi").Get()
	cpuOnly := []corev1.ResourceName{corev1.ResourceCPU}
	vpa.Spec.ResourcePolicy = &vpa_types.PodResourcePolicy{ContainerPolicies: []vpa_types.ContainerResourcePolicy{{
		ContainerName:       containerName,
		ControlledResources: &cpuOnly,
	}}}
	got, err := newTestCalculator().Compute(podWith("1", "1Gi"), vpa)
	require.NoError(t, err)
	require.NotNil(t, got)
	assertHintEqual(t, &corev1.PodResourceHint{
		Manager:     ManagerName,
		UpdateTime:  metav1.NewTime(fixedNow),
		Replacement: vals(rl("cpu", "2")),
	}, got)
}

func TestNeedsWrite(t *testing.T) {
	hint := func(kv ...string) *corev1.PodResourceHint {
		return &corev1.PodResourceHint{Manager: ManagerName, Replacement: vals(rl(kv...))}
	}
	base := hint("cpu", "1")
	tests := []struct {
		name     string
		existing *corev1.PodResourceHint
		desired  *corev1.PodResourceHint
		reason   Reason
		want     bool
	}{
		{name: "nothing to do", want: false},
		{name: "new entry", desired: base, want: true},
		{name: "remove entry", existing: base, want: true},
		{name: "unchanged", existing: base, desired: hint("cpu", "1"), want: false},
		{name: "small increase under threshold", existing: base, desired: hint("cpu", "1050m"), want: true},
		{name: "small decrease under threshold", existing: base, desired: hint("cpu", "950m"), want: false},
		{name: "decrease over threshold", existing: base, desired: hint("cpu", "500m"), want: true},
		{name: "increase over threshold", existing: base, desired: hint("cpu", "2"), want: true},
		{name: "new resource", existing: base, desired: hint("cpu", "1", "memory", "1Gi"), want: true},
		{name: "resource dropped", existing: hint("cpu", "1", "memory", "1Gi"), desired: base, want: true},
		{name: "resize forces write", existing: base, desired: hint("cpu", "950m"), reason: ReasonResized, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NeedsWrite(tc.existing, tc.desired, 0.1, tc.reason))
		})
	}
}

func assertHintEqual(t *testing.T, want, got *corev1.PodResourceHint) {
	t.Helper()
	assert.Equal(t, want.Manager, got.Manager)
	assert.True(t, want.UpdateTime.Equal(&got.UpdateTime), "updateTime")
	assertValuesEqual(t, "replacement", want.Replacement, got.Replacement)
}

func assertValuesEqual(t *testing.T, field string, want, got *corev1.ResourceHintValues) {
	t.Helper()
	if want == nil {
		assert.Nil(t, got, field)
		return
	}
	require.NotNil(t, got, field)
	wf, gf := flatten(want), flatten(got)
	require.Len(t, gf, len(wf), field)
	for k, wq := range wf {
		gq, ok := gf[k]
		require.True(t, ok, "%s missing %v", field, k)
		assert.Zero(t, wq.Cmp(gq), "%s %v: want %s got %s", field, k, wq.String(), gq.String())
	}
}
