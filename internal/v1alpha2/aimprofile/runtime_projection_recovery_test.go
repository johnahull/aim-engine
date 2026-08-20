// MIT License
//
// Copyright (c) 2025 Advanced Micro Devices, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package aimprofile

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/profileyaml"
)

type profileProjectionRecoveryReconciler struct {
	delegate    *ProfileReconciler
	observation ProfileObservation
}

func (r *profileProjectionRecoveryReconciler) FetchRemoteState(
	_ context.Context,
	_ client.Client,
	_ controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile],
) ProfileFetchResult {
	return r.observation.ProfileFetchResult
}

func (r *profileProjectionRecoveryReconciler) ComposeState(
	_ context.Context,
	_ controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile],
	_ ProfileFetchResult,
) ProfileObservation {
	return r.observation
}

func (*profileProjectionRecoveryReconciler) PlanResources(
	_ context.Context,
	_ controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile],
	_ ProfileObservation,
) controllerutils.PlanResult {
	return controllerutils.PlanResult{}
}

func (r *profileProjectionRecoveryReconciler) DecorateStatus(
	status *aimv1alpha2.AIMProfileStatus,
	cm *controllerutils.ConditionManager,
	obs ProfileObservation,
) {
	r.delegate.DecorateStatus(status, cm, obs)
}

type clusterProfileProjectionRecoveryReconciler struct {
	delegate    *ClusterProfileReconciler
	observation ClusterProfileObservation
}

func (r *clusterProfileProjectionRecoveryReconciler) FetchRemoteState(
	_ context.Context,
	_ client.Client,
	_ controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile],
) ClusterProfileFetchResult {
	return r.observation.ClusterProfileFetchResult
}

func (r *clusterProfileProjectionRecoveryReconciler) ComposeState(
	_ context.Context,
	_ controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile],
	_ ClusterProfileFetchResult,
) ClusterProfileObservation {
	return r.observation
}

func (*clusterProfileProjectionRecoveryReconciler) PlanResources(
	_ context.Context,
	_ controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile],
	_ ClusterProfileObservation,
) controllerutils.PlanResult {
	return controllerutils.PlanResult{}
}

func (r *clusterProfileProjectionRecoveryReconciler) DecorateStatus(
	status *aimv1alpha2.AIMProfileStatus,
	cm *controllerutils.ConditionManager,
	obs ClusterProfileObservation,
) {
	r.delegate.DecorateStatus(status, cm, obs)
}

func TestProfileRuntimeProjectionRecoversAfterContractBackfill(t *testing.T) {
	ctx := context.Background()
	spec := projectionRecoverySpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-profile",
			Namespace: "team-a",
			Labels: map[string]string{
				constants.LabelKeyProfileOrigin: string(aimv1alpha1.ProfileOriginDiscovered),
			},
		},
		Spec: aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	delegate := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	reconciler := &profileProjectionRecoveryReconciler{delegate: delegate}
	reconciler.observation = delegate.ComposeState(
		ctx,
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		ProfileFetchResult{profile: profile},
	)
	if reconciler.observation.projectionErr == nil {
		t.Fatal("cycle 1: expected missing contract to fail runtime projection")
	}

	scheme := runtime.NewScheme()
	if err := aimv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("add AIM API to scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&aimv1alpha2.AIMProfile{}).
		Build()
	if err := cl.Create(ctx, profile); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	pipeline := controllerutils.Pipeline[
		*aimv1alpha2.AIMProfile,
		*aimv1alpha2.AIMProfileStatus,
		ProfileFetchResult,
		ProfileObservation,
	]{
		Client:         cl,
		StatusClient:   cl.Status(),
		Recorder:       record.NewFakeRecorder(20),
		ControllerName: "profile-recovery-test",
		Reconciler:     reconciler,
		Scheme:         scheme,
		Clientset:      kubernetesfake.NewSimpleClientset(),
	}

	if _, err := pipeline.Run(ctx, profile); err != nil {
		t.Fatalf("cycle 1: pipeline run: %v", err)
	}
	requireCondition(t, profile.Status.Conditions, runtimeProjectionReadyCondition).assert(
		t,
		metav1.ConditionFalse,
		aimv1alpha2.AIMProfileReasonRuntimeProjectionFailed,
	)
	requireCondition(t, profile.Status.Conditions, controllerutils.ConditionTypeReady).assertStatus(t, metav1.ConditionFalse)

	profile.Annotations = profileyaml.Mark(
		profile.Annotations,
		profileyaml.CanonicalContract(&profile.Spec.AIMProfileSpecCommon),
	)
	reconciler.observation = delegate.ComposeState(
		ctx,
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		ProfileFetchResult{profile: profile},
	)
	if reconciler.observation.projectionErr != nil {
		t.Fatalf("cycle 2: projection after contract backfill: %v", reconciler.observation.projectionErr)
	}

	if _, err := pipeline.Run(ctx, profile); err != nil {
		t.Fatalf("cycle 2: pipeline run: %v", err)
	}
	if got := findCondition(profile.Status.Conditions, runtimeProjectionReadyCondition); got != nil {
		t.Fatalf("cycle 2: stale %s condition retained: %+v", runtimeProjectionReadyCondition, got)
	}
	requireCondition(t, profile.Status.Conditions, aimv1alpha2.AIMProfileConditionRuntimeProjected).assert(
		t,
		metav1.ConditionTrue,
		aimv1alpha2.AIMProfileReasonRuntimeProjected,
	)
	requireCondition(t, profile.Status.Conditions, controllerutils.ConditionTypeConfigValid).assertStatus(t, metav1.ConditionTrue)
	requireCondition(t, profile.Status.Conditions, controllerutils.ConditionTypeReady).assertStatus(t, metav1.ConditionTrue)
	if profile.Status.Status != constants.AIMStatusReady {
		t.Fatalf("cycle 2: status = %q, want %q", profile.Status.Status, constants.AIMStatusReady)
	}
}

func TestClusterProfileRuntimeProjectionRecoversAfterContractBackfill(t *testing.T) {
	ctx := context.Background()
	spec := projectionRecoverySpec()
	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-cluster-profile",
			Labels: map[string]string{
				constants.LabelKeyProfileOrigin: string(aimv1alpha1.ProfileOriginDiscovered),
			},
		},
		Spec: aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
	}
	delegate := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	reconciler := &clusterProfileProjectionRecoveryReconciler{delegate: delegate}
	reconciler.observation = delegate.ComposeState(
		ctx,
		controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: profile},
		ClusterProfileFetchResult{profile: profile},
	)
	if reconciler.observation.projectionErr == nil {
		t.Fatal("cycle 1: expected missing contract to fail runtime projection")
	}

	scheme := runtime.NewScheme()
	if err := aimv1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("add AIM API to scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&aimv1alpha2.AIMClusterProfile{}).
		Build()
	if err := cl.Create(ctx, profile); err != nil {
		t.Fatalf("create cluster profile: %v", err)
	}
	pipeline := controllerutils.Pipeline[
		*aimv1alpha2.AIMClusterProfile,
		*aimv1alpha2.AIMProfileStatus,
		ClusterProfileFetchResult,
		ClusterProfileObservation,
	]{
		Client:         cl,
		StatusClient:   cl.Status(),
		Recorder:       record.NewFakeRecorder(20),
		ControllerName: "cluster-profile-recovery-test",
		Reconciler:     reconciler,
		Scheme:         scheme,
		Clientset:      kubernetesfake.NewSimpleClientset(),
	}

	if _, err := pipeline.Run(ctx, profile); err != nil {
		t.Fatalf("cycle 1: pipeline run: %v", err)
	}
	requireCondition(t, profile.Status.Conditions, runtimeProjectionReadyCondition).assert(
		t,
		metav1.ConditionFalse,
		aimv1alpha2.AIMProfileReasonRuntimeProjectionFailed,
	)
	requireCondition(t, profile.Status.Conditions, controllerutils.ConditionTypeReady).assertStatus(t, metav1.ConditionFalse)

	profile.Annotations = profileyaml.Mark(
		profile.Annotations,
		profileyaml.CanonicalContract(&profile.Spec.AIMProfileSpecCommon),
	)
	reconciler.observation = delegate.ComposeState(
		ctx,
		controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: profile},
		ClusterProfileFetchResult{profile: profile},
	)
	if reconciler.observation.projectionErr != nil {
		t.Fatalf("cycle 2: projection after contract backfill: %v", reconciler.observation.projectionErr)
	}

	if _, err := pipeline.Run(ctx, profile); err != nil {
		t.Fatalf("cycle 2: pipeline run: %v", err)
	}
	if got := findCondition(profile.Status.Conditions, runtimeProjectionReadyCondition); got != nil {
		t.Fatalf("cycle 2: stale %s condition retained: %+v", runtimeProjectionReadyCondition, got)
	}
	requireCondition(t, profile.Status.Conditions, aimv1alpha2.AIMProfileConditionRuntimeProjected).assert(
		t,
		metav1.ConditionTrue,
		aimv1alpha2.AIMProfileReasonRuntimeProjected,
	)
	requireCondition(t, profile.Status.Conditions, controllerutils.ConditionTypeConfigValid).assertStatus(t, metav1.ConditionTrue)
	requireCondition(t, profile.Status.Conditions, controllerutils.ConditionTypeReady).assertStatus(t, metav1.ConditionTrue)
	if profile.Status.Status != constants.AIMStatusReady {
		t.Fatalf("cycle 2: status = %q, want %q", profile.Status.Status, constants.AIMStatusReady)
	}
}

func projectionRecoverySpec() aimv1alpha2.AIMProfileSpecCommon {
	return aimv1alpha2.AIMProfileSpecCommon{
		AimId:   "org/model",
		ModelId: "org/model",
		Image:   "registry.io/model:1.0.0",
		Engine:  "vllm",
		ModelSources: []aimv1alpha1.AIMModelSource{{
			ModelID:   "org/model",
			SourceURI: "hf://org/model",
		}},
	}
}

type conditionAssertion struct {
	condition *metav1.Condition
}

func requireCondition(t *testing.T, conditions []metav1.Condition, conditionType string) conditionAssertion {
	t.Helper()
	condition := findCondition(conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q not found in %+v", conditionType, conditions)
	}
	return conditionAssertion{condition: condition}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func (a conditionAssertion) assert(t *testing.T, status metav1.ConditionStatus, reason string) {
	t.Helper()
	a.assertStatus(t, status)
	if a.condition.Reason != reason {
		t.Fatalf("condition %q reason = %q, want %q", a.condition.Type, a.condition.Reason, reason)
	}
}

func (a conditionAssertion) assertStatus(t *testing.T, status metav1.ConditionStatus) {
	t.Helper()
	if a.condition.Status != status {
		t.Fatalf("condition %q status = %q, want %q", a.condition.Type, a.condition.Status, status)
	}
}
