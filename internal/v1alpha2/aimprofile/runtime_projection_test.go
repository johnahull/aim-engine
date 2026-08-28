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
	"fmt"
	"testing"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/profileyaml"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/runtimeprojection"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

// gpuSpec returns a deployable GPU profile spec carrying an image, used as the
// projectable baseline across the projection tests.
func gpuSpec() aimv1alpha2.AIMProfileSpecCommon {
	return aimv1alpha2.AIMProfileSpecCommon{
		AimId:            "qwen/qwen3-32b",
		Image:            "registry.io/qwen3-32b:1.0.0",
		AcceleratorModel: "MI300X",
		AcceleratorType:  aimv1alpha2.AcceleratorTypeGPU,
		AcceleratorCount: 1,
		ModelSources:     nil,
	}
}

// findRuntime returns the single ServingRuntime in objs, failing otherwise.
func findServingRuntime(t *testing.T, objs []client.Object) *kservev1alpha1.ServingRuntime {
	t.Helper()
	var found *kservev1alpha1.ServingRuntime
	for _, o := range objs {
		if sr, ok := o.(*kservev1alpha1.ServingRuntime); ok {
			if found != nil {
				t.Fatalf("expected exactly one ServingRuntime, found multiple")
			}
			found = sr
		}
	}
	if found == nil {
		t.Fatalf("expected a ServingRuntime, found none in %d objects", len(objs))
	}
	return found
}

func findClusterServingRuntime(t *testing.T, objs []client.Object) *kservev1alpha1.ClusterServingRuntime {
	t.Helper()
	var found *kservev1alpha1.ClusterServingRuntime
	for _, o := range objs {
		if csr, ok := o.(*kservev1alpha1.ClusterServingRuntime); ok {
			if found != nil {
				t.Fatalf("expected exactly one ClusterServingRuntime, found multiple")
			}
			found = csr
		}
	}
	if found == nil {
		t.Fatalf("expected a ClusterServingRuntime, found none in %d objects", len(objs))
	}
	return found
}

func countServingRuntimes(objs []client.Object) int {
	n := 0
	for _, o := range objs {
		if _, ok := o.(*kservev1alpha1.ServingRuntime); ok {
			n++
		}
	}
	return n
}

func countClusterServingRuntimes(objs []client.Object) int {
	n := 0
	for _, o := range objs {
		if _, ok := o.(*kservev1alpha1.ClusterServingRuntime); ok {
			n++
		}
	}
	return n
}

// TestRuntimeProjectable pins the projectable gate: deployable && image != "" &&
// (no accelerator || matchingNodes > 0). Each clause is exercised independently.
func TestRuntimeProjectable(t *testing.T) {
	t.Parallel()
	deployableGPU := gpuSpec()
	cpu := aimv1alpha2.AIMProfileSpecCommon{AimId: "m", Image: "registry.io/cpu:1.0.0", ModelSources: nil}

	cases := []struct {
		name       string
		spec       aimv1alpha2.AIMProfileSpecCommon
		deployable bool
		match      NodeMatchResult
		want       bool
	}{
		{"gpu deployable with matching nodes", deployableGPU, true, NodeMatchResult{MatchingNodes: 2}, true},
		{"gpu deployable no matching nodes", deployableGPU, true, NodeMatchResult{MatchingNodes: 0}, false},
		{"gpu not deployable", deployableGPU, false, NodeMatchResult{MatchingNodes: 2}, false},
		{"no image", func() aimv1alpha2.AIMProfileSpecCommon {
			s := gpuSpec()
			s.Image = ""
			return s
		}(), true, NodeMatchResult{MatchingNodes: 2}, false},
		{"cpu profile needs no nodes", cpu, true, NodeMatchResult{}, true},
		{"cpu profile not deployable", cpu, false, NodeMatchResult{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runtimeProjectable(tc.spec, tc.deployable, tc.match); got != tc.want {
				t.Fatalf("runtimeProjectable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProfilePlanResources_ProjectsNamespaceRuntime pins the Exhaustive
// namespace projection: a projectable AIMProfile yields exactly one
// ServingRuntime (+ its colocated ConfigMap), named aim-<profile>, routed
// through the force-apply bucket with the right image, affinity, and labels.
func TestProfilePlanResources_ProjectsNamespaceRuntime(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "team-a"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	affinity := BuildNodeAffinity(aimv1alpha2.AcceleratorTypeGPU, "MI300X", "unpartitioned")
	obs := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 3, NodeAffinity: affinity},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       true,
		yamlContract:      profileyaml.CanonicalContract(&spec),
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)

	// Runtime + ConfigMap go through the authoritative force-apply bucket; the
	// default bucket must stay empty (no profile-owned cache here).
	if got := len(plan.GetToApply()); got != 0 {
		t.Fatalf("expected no default-bucket objects, got %d", got)
	}
	force := plan.GetToApplyWithForce()
	sr := findServingRuntime(t, force)

	wantName := serving.RuntimeName(profile.Name)
	if sr.Name != wantName {
		t.Errorf("runtime name = %q, want %q", sr.Name, wantName)
	}
	if sr.Namespace != profile.Namespace {
		t.Errorf("runtime namespace = %q, want %q", sr.Namespace, profile.Namespace)
	}
	if got := sr.Spec.Containers[0].Image; got != spec.Image {
		t.Errorf("runtime image = %q, want %q", got, spec.Image)
	}
	if sr.Spec.Affinity == nil || sr.Spec.Affinity.NodeAffinity == nil {
		t.Errorf("runtime must carry resolved node affinity")
	}
	if sr.Labels[constants.LabelK8sManagedBy] != constants.LabelValueManagedBy {
		t.Errorf("runtime missing managed-by label: %v", sr.Labels)
	}
	if sr.Labels[constants.LabelProfile] == "" {
		t.Errorf("runtime missing profile correlator label: %v", sr.Labels)
	}
	if sr.Labels[constants.LabelKeyAcceleratorClass] != "MI300X" {
		t.Errorf("runtime missing accelerator-class label: %v", sr.Labels)
	}
	// supportedModelFormat autoSelect must be OFF for per-profile runtimes.
	if len(sr.Spec.SupportedModelFormats) == 0 || sr.Spec.SupportedModelFormats[0].AutoSelect == nil || *sr.Spec.SupportedModelFormats[0].AutoSelect {
		t.Errorf("per-profile runtime must have autoSelect=false")
	}
	// Framework env must carry AIM_PROFILE_ID.
	if !hasEnv(sr.Spec.Containers[0].Env, constants.EnvAIMProfileID) {
		t.Errorf("runtime missing %s framework env", constants.EnvAIMProfileID)
	}
	// The colocated ConfigMap is also force-applied and named after the runtime.
	cm := findConfigMap(t, force, wantName, profile.Namespace)

	// AIMService and the lazy projection controller rebuild expected content
	// from profile status. That independently-built projection must have exactly
	// the same hash as the eager producer or the freshness gate cannot converge.
	profile.Status.Resources = obs.resolvedResources
	profile.Status.ResolvedNodeAffinity = affinity
	lazy, err := runtimeprojection.DesiredForRuntime(
		profile.Namespace,
		wantName,
		runtimeprojection.ProjectionState{AnnotatedProfile: profile},
	)
	if err != nil {
		t.Fatalf("build equivalent lazy projection: %v", err)
	}
	if lazy.Runtime == nil || lazy.ConfigMap == nil {
		t.Fatal("equivalent lazy projection did not produce both siblings")
	}
	for name, got := range map[string]string{
		"eager ServingRuntime": sr.Annotations[constants.AnnotationRuntimeProjectionContentHash],
		"eager ConfigMap":      cm.Annotations[constants.AnnotationRuntimeProjectionContentHash],
		"lazy ServingRuntime":  lazy.Runtime.Annotations[constants.AnnotationRuntimeProjectionContentHash],
		"lazy ConfigMap":       lazy.ConfigMap.Annotations[constants.AnnotationRuntimeProjectionContentHash],
	} {
		if got != sr.Annotations[constants.AnnotationRuntimeProjectionContentHash] {
			t.Errorf("%s content hash = %q, want %q", name, got, sr.Annotations[constants.AnnotationRuntimeProjectionContentHash])
		}
	}
}

func TestProfileRuntimeProjectionBuildErrorSurfacesInStatusAndSkipsApply(t *testing.T) {
	spec := gpuSpec()
	spec.Engine = "vllm"
	spec.AcceleratorVendor = aimv1alpha1.AcceleratorVendorNVIDIA
	spec.ModelId = "org/model"
	spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}
	spec.EngineArgs = &apiextensionsv1.JSON{Raw: []byte(`{"port": 9000}`)}
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-vllm", Namespace: "team-a"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 1},
		resolvedResources: ResolveProfileResources(spec),
		deployable:        true,
		projectable:       true,
	}
	obs.profile = profile
	obs.projectionErr = validateNamespaceRuntimeProjection(
		aimv1alpha2.RuntimeProjectionModeBoth,
		profile,
		obs,
	)
	if obs.projectionErr == nil {
		t.Fatal("expected invalid vLLM arguments to fail runtime construction")
	}

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeBoth}
	plan := r.PlanResources(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		obs,
	)
	if got := len(plan.GetToApplyWithForce()); got != 0 {
		t.Fatalf("invalid runtime must not be applied, got %d force-applied objects", got)
	}

	status := &aimv1alpha2.AIMProfileStatus{}
	cm := controllerutils.NewConditionManager(nil)
	r.DecorateStatus(status, cm, obs)
	condition := cm.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected)
	if condition == nil ||
		condition.Status != metav1.ConditionFalse ||
		condition.Reason != aimv1alpha2.AIMProfileReasonRuntimeProjectionFailed {
		t.Fatalf("RuntimeProjected condition = %+v, want False/%s", condition, aimv1alpha2.AIMProfileReasonRuntimeProjectionFailed)
	}
	if status.ProjectedRuntimeName != "" || status.ProjectedModelSlugRuntimeName != "" {
		t.Fatalf("failed first projection must not publish runtime names: %+v", status)
	}

	staleStatus := &aimv1alpha2.AIMProfileStatus{
		ProjectedRuntimeName:          serving.RuntimeName(profile.Name),
		ProjectedModelSlugRuntimeName: serving.ModelSlugRuntimeName(spec.AimId),
	}
	r.DecorateStatus(staleStatus, controllerutils.NewConditionManager(nil), obs)
	if staleStatus.ProjectedRuntimeName == "" || staleStatus.ProjectedModelSlugRuntimeName == "" {
		t.Fatalf("builder failure must retain discoverability for stale runtimes kept by additive teardown: %+v", staleStatus)
	}

	health := obs.GetComponentHealth(context.Background(), nil)
	var projectionHealth *controllerutils.ComponentHealth
	for i := range health {
		if health[i].Component == "RuntimeProjection" {
			projectionHealth = &health[i]
			break
		}
	}
	if projectionHealth == nil || len(projectionHealth.Errors) != 1 {
		t.Fatalf("missing runtime projection component health: %+v", health)
	}
	if got := controllerutils.CategorizeError(projectionHealth.Errors[0]).Category(); got != controllerutils.ErrorCategoryInvalidSpec {
		t.Fatalf("projection error category = %v, want InvalidSpec", got)
	}
}

func TestSourceDerivedProfileMissingContractKeepsExistingProjectionUntouched(t *testing.T) {
	spec := aimv1alpha2.AIMProfileSpecCommon{
		AimId:   "org/model",
		ModelId: "org/model",
		Image:   "registry.io/model:1.0.0",
		Engine:  "vllm",
		ModelSources: []aimv1alpha1.AIMModelSource{{
			ModelID:   "org/model",
			SourceURI: "hf://org/model",
		}},
	}
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
	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	obs := r.ComposeState(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		ProfileFetchResult{profile: profile},
	)
	if obs.projectionErr == nil {
		t.Fatal("expected missing source-derived contract to block projection")
	}

	plan := r.PlanResources(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		obs,
	)
	if len(plan.GetToApplyWithForce()) != 0 || len(plan.GetToDelete()) != 0 {
		t.Fatalf("missing contract must leave existing projection untouched: apply=%d delete=%d",
			len(plan.GetToApplyWithForce()), len(plan.GetToDelete()))
	}

	status := &aimv1alpha2.AIMProfileStatus{ProjectedRuntimeName: serving.RuntimeName(profile.Name)}
	r.DecorateStatus(status, controllerutils.NewConditionManager(nil), obs)
	if status.ProjectedRuntimeName == "" {
		t.Fatal("missing contract must retain the previously published runtime name")
	}
}

func TestClusterProfileRuntimeProjectionBuildErrorSkipsApply(t *testing.T) {
	spec := gpuSpec()
	spec.Engine = "vllm"
	spec.AcceleratorVendor = aimv1alpha1.AcceleratorVendorNVIDIA
	spec.ModelId = "org/model"
	spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}
	spec.EngineArgs = &apiextensionsv1.JSON{Raw: []byte(`{"host": "127.0.0.1"}`)}
	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-vllm"},
		Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ClusterProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 1},
		resolvedResources: ResolveProfileResources(spec),
		deployable:        true,
		projectable:       true,
	}
	obs.profile = profile
	obs.projectionErr = validateClusterRuntimeProjection(
		aimv1alpha2.RuntimeProjectionModeBoth,
		profile,
		obs,
	)
	if obs.projectionErr == nil {
		t.Fatal("expected invalid vLLM arguments to fail cluster runtime construction")
	}

	r := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeBoth}
	plan := r.PlanResources(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: profile},
		obs,
	)
	if got := len(plan.GetToApplyWithForce()); got != 0 {
		t.Fatalf("invalid cluster runtime must not be applied, got %d force-applied objects", got)
	}
}

// findConfigMap returns the single ConfigMap in objs matching name/namespace,
// failing if none is present.
func findConfigMap(t *testing.T, objs []client.Object, name, namespace string) *corev1.ConfigMap {
	t.Helper()
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == name && cm.Namespace == namespace {
			return cm
		}
	}
	t.Fatalf("expected ConfigMap %s/%s in %d objects", namespace, name, len(objs))
	return nil
}

// TestProfilePlanResources_StampsEagerProjectionMarker: the eager per-profile
// (Exhaustive) and model-slug (Reduced) projections must stamp the eager marker
// on both the ServingRuntime and its colocated ConfigMap. That marker is what
// lets the eager force-apply reclaim a same-named lazy shadow so ownership
// settles on a mode flip — so it must never be the lazy value.
func TestProfilePlanResources_StampsEagerProjectionMarker(t *testing.T) {
	cases := []struct {
		name     string
		spec     aimv1alpha2.AIMProfileSpecCommon
		mode     aimv1alpha2.RuntimeProjectionMode
		wantName func(profileName string, spec aimv1alpha2.AIMProfileSpecCommon) string
	}{
		{
			name: "exhaustive per-profile runtime",
			spec: gpuSpec(),
			mode: aimv1alpha2.RuntimeProjectionModeExhaustive,
			wantName: func(profileName string, _ aimv1alpha2.AIMProfileSpecCommon) string {
				return serving.RuntimeName(profileName)
			},
		},
		{
			name: "reduced model-slug primary runtime",
			spec: primaryGPUSpec(),
			mode: aimv1alpha2.RuntimeProjectionModeReduced,
			wantName: func(_ string, spec aimv1alpha2.AIMProfileSpecCommon) string {
				return serving.ModelSlugRuntimeName(spec.AimId)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := &aimv1alpha2.AIMProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "team-a"},
				Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: tc.spec},
			}
			affinity := BuildNodeAffinity(aimv1alpha2.AcceleratorTypeGPU, "MI300X", "unpartitioned")
			obs := ProfileObservation{
				ProfileFetchResult: ProfileFetchResult{modelSlugWinner: tc.mode.ProjectsModelSlug()},
				matchResult:        NodeMatchResult{MatchingNodes: 3, NodeAffinity: affinity},
				resolvedResources:  ResolveResources(tc.spec.AcceleratorType, tc.spec.AcceleratorCount, nil, tc.spec.AcceleratorModel, nil),
				deployable:         true,
				projectable:        true,
				yamlContract:       profileyaml.CanonicalContract(&tc.spec),
			}
			obs.profile = profile

			r := &ProfileReconciler{ProjectionMode: tc.mode}
			plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)

			force := plan.GetToApplyWithForce()
			sr := findServingRuntime(t, force)
			wantName := tc.wantName(profile.Name, tc.spec)
			if sr.Name != wantName {
				t.Fatalf("runtime name = %q, want %q", sr.Name, wantName)
			}
			if got := sr.Labels[constants.LabelRuntimeProjection]; got != constants.LabelValueRuntimeProjectionEager {
				t.Errorf("runtime %s label = %q, want %q", constants.LabelRuntimeProjection, got, constants.LabelValueRuntimeProjectionEager)
			}
			if sr.Labels[constants.LabelRuntimeProjection] == constants.LabelValueRuntimeProjectionLazy {
				t.Errorf("eager runtime must never carry the lazy marker")
			}

			cm := findConfigMap(t, force, wantName, profile.Namespace)
			if got := cm.Labels[constants.LabelRuntimeProjection]; got != constants.LabelValueRuntimeProjectionEager {
				t.Errorf("colocated ConfigMap %s label = %q, want %q", constants.LabelRuntimeProjection, got, constants.LabelValueRuntimeProjectionEager)
			}
		})
	}
}

// TestProfilePlanResources_GatesBlockProjection pins that no namespace runtime
// is projected when any projectable clause fails independently.
func TestProfilePlanResources_GatesBlockProjection(t *testing.T) {
	cases := map[string]struct {
		spec       aimv1alpha2.AIMProfileSpecCommon
		deployable bool
		match      NodeMatchResult
	}{
		"no image": func() struct {
			spec       aimv1alpha2.AIMProfileSpecCommon
			deployable bool
			match      NodeMatchResult
		} {
			s := gpuSpec()
			s.Image = ""
			return struct {
				spec       aimv1alpha2.AIMProfileSpecCommon
				deployable bool
				match      NodeMatchResult
			}{s, true, NodeMatchResult{MatchingNodes: 2}}
		}(),
		"no matching nodes": {gpuSpec(), true, NodeMatchResult{MatchingNodes: 0}},
		"not deployable":    {gpuSpec(), false, NodeMatchResult{MatchingNodes: 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			profile := &aimv1alpha2.AIMProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
				Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: tc.spec},
			}
			obs := ProfileObservation{
				matchResult:       tc.match,
				resolvedResources: ResolveResources(tc.spec.AcceleratorType, tc.spec.AcceleratorCount, nil, tc.spec.AcceleratorModel, nil),
				deployable:        tc.deployable,
				projectable:       runtimeProjectable(tc.spec, tc.deployable, tc.match),
			}
			obs.profile = profile

			r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
			plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)
			if n := countServingRuntimes(plan.GetToApplyWithForce()); n != 0 {
				t.Fatalf("expected no runtime when gate fails, got %d", n)
			}
		})
	}
}

// TestProfilePlanResources_AsymmetricTeardownIsAdditive pins the additive
// asymmetric teardown: a gate-false profile that never projected emits nothing,
// while a retained projection is reasserted so its content cannot go stale.
// Neither case deletes; removal happens only via ownerRef GC on profile delete.
func TestProfilePlanResources_AsymmetricTeardownIsAdditive(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 0}, // gate now false (nodes vanished)
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       false,
		yamlContract:      profileyaml.CanonicalContract(&spec),
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)
	if n := countServingRuntimes(plan.GetToApplyWithForce()); n != 0 {
		t.Fatalf("additive teardown: never-projected gate-false profile emitted %d runtimes", n)
	}
	if n := len(plan.GetToApply()); n != 0 {
		t.Fatalf("additive teardown: gate-false must not queue the runtime for apply, got %d", n)
	}
	if n := len(plan.GetToDelete()); n != 0 {
		t.Fatalf("asymmetric teardown must never delete the runtime, got %d deletes", n)
	}

	profile.Status.ProjectedRuntimeName = serving.RuntimeName(profile.Name)
	obs = r.ComposeState(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		ProfileFetchResult{profile: profile},
	)
	if obs.projectable {
		t.Fatal("test precondition: retained profile must no longer be projectable")
	}
	if obs.projectionErr != nil {
		t.Fatalf("retained projection contract/build validation failed: %v", obs.projectionErr)
	}
	if err := obs.yamlContract.Validate(); err != nil {
		t.Fatalf("retained projection must compose a valid YAML contract: %v", err)
	}
	plan = r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)
	if n := countServingRuntimes(plan.GetToApplyWithForce()); n != 1 {
		t.Fatalf("additive teardown: retained projection must be reasserted, got %d runtimes", n)
	}
	if n := len(plan.GetToDelete()); n != 0 {
		t.Fatalf("asymmetric teardown must never delete the retained runtime, got %d deletes", n)
	}
}

// TestClusterProfilePlanResources_ProjectsClusterRuntime pins the Exhaustive
// cluster projection: a projectable AIMClusterProfile yields exactly one bare
// ClusterServingRuntime (no ConfigMap) named aim-<profile>, force-applied.
func TestClusterProfilePlanResources_ProjectsClusterRuntime(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x"},
		Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
	}
	affinity := BuildNodeAffinity(aimv1alpha2.AcceleratorTypeGPU, "MI300X", "unpartitioned")
	obs := ClusterProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 3, NodeAffinity: affinity},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       true,
	}
	obs.profile = profile

	r := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: profile}, obs)

	if got := len(plan.GetToApply()); got != 0 {
		t.Fatalf("cluster profile must not use the default bucket, got %d", got)
	}
	force := plan.GetToApplyWithForce()
	csr := findClusterServingRuntime(t, force)
	if csr.Name != serving.RuntimeName(profile.Name) {
		t.Errorf("runtime name = %q, want %q", csr.Name, serving.RuntimeName(profile.Name))
	}
	if got := csr.Spec.Containers[0].Image; got != spec.Image {
		t.Errorf("runtime image = %q, want %q", got, spec.Image)
	}
	if csr.Spec.Affinity == nil || csr.Spec.Affinity.NodeAffinity == nil {
		t.Errorf("cluster runtime must carry resolved node affinity")
	}
	if csr.Labels[constants.LabelProfile] == "" {
		t.Errorf("cluster runtime missing profile correlator label: %v", csr.Labels)
	}
	// A bare CSR carries no colocated ConfigMap.
	for _, o := range force {
		if _, ok := o.(*corev1.ConfigMap); ok {
			t.Errorf("bare ClusterServingRuntime must not project a ConfigMap")
		}
	}
}

// TestClusterProfilePlanResources_GatesAndTeardown pins gate-blocking and
// additive asymmetric teardown for the cluster path: a never-projected profile
// emits nothing after a gate failure, while a retained CSR is reasserted and
// never explicitly deleted.
func TestClusterProfilePlanResources_GatesAndTeardown(t *testing.T) {
	spec := gpuSpec()
	build := func(projectable bool) (*ClusterProfileReconciler, controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile], ClusterProfileObservation) {
		profile := &aimv1alpha2.AIMClusterProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "p"},
			Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
		}
		obs := ClusterProfileObservation{
			resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
			deployable:        true,
			projectable:       projectable,
		}
		obs.profile = profile
		r := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
		return r, controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: profile}, obs
	}

	t.Run("gate true projects the cluster runtime", func(t *testing.T) {
		r, rc, obs := build(true)
		plan := r.PlanResources(context.Background(), rc, obs)
		if n := countClusterServingRuntimes(plan.GetToApplyWithForce()); n != 1 {
			t.Fatalf("expected the cluster runtime to be projected, got %d", n)
		}
	})

	t.Run("never-projected gate false stops emitting and never deletes", func(t *testing.T) {
		r, rc, obs := build(false)
		plan := r.PlanResources(context.Background(), rc, obs)
		if n := countClusterServingRuntimes(plan.GetToApplyWithForce()); n != 0 {
			t.Fatalf("additive teardown: gate-false must stop emitting, got %d applies", n)
		}
		if n := len(plan.GetToDelete()); n != 0 {
			t.Fatalf("must never delete the runtime, got %d deletes", n)
		}
	})

	t.Run("retained gate-false projection is reasserted after switch to Reduced", func(t *testing.T) {
		r, rc, _ := build(false)
		r.ProjectionMode = aimv1alpha2.RuntimeProjectionModeReduced
		rc.Object.Status.ProjectedRuntimeName = serving.RuntimeName(rc.Object.Name)
		obs := r.ComposeState(
			context.Background(),
			rc,
			ClusterProfileFetchResult{profile: rc.Object},
		)
		if obs.projectable {
			t.Fatal("test precondition: retained cluster profile must no longer be projectable")
		}
		if obs.projectionErr != nil {
			t.Fatalf("retained cluster projection validation failed: %v", obs.projectionErr)
		}
		plan := r.PlanResources(context.Background(), rc, obs)
		if n := countClusterServingRuntimes(plan.GetToApplyWithForce()); n != 1 {
			t.Fatalf("retained cluster runtime must be reasserted, got %d applies", n)
		}
		if n := len(plan.GetToDelete()); n != 0 {
			t.Fatalf("must never delete the retained runtime, got %d deletes", n)
		}
	})
}

// TestReducedModeCreatesNoNewPerProfileRuntimeButMaintainsRetained pins the
// additive mode-flip lifecycle: Reduced does not create a per-profile runtime,
// but it keeps reasserting one previously published by Exhaustive/Both so the
// eager marker never strands stale content that the lazy controller defers to.
func TestReducedModeCreatesNoNewPerProfileRuntimeButMaintainsRetained(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{deployable: true, projectable: true}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)
	if n := countServingRuntimes(plan.GetToApplyWithForce()); n != 0 {
		t.Fatalf("Reduced mode must not project per-profile runtimes for a non-primary profile, got %d", n)
	}

	profile.Status.ProjectedRuntimeName = serving.RuntimeName(profile.Name)
	obs = r.ComposeState(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		ProfileFetchResult{profile: profile},
	)
	if obs.projectionErr != nil {
		t.Fatalf("compose retained Reduced-mode projection: %v", obs.projectionErr)
	}
	plan = r.PlanResources(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile},
		obs,
	)
	if n := countServingRuntimes(plan.GetToApplyWithForce()); n != 1 {
		t.Fatalf("Reduced mode must maintain the retained per-profile runtime, got %d applies", n)
	}
	if runtime := findServingRuntime(t, plan.GetToApplyWithForce()); runtime.Name != profile.Status.ProjectedRuntimeName {
		t.Fatalf("maintained runtime name = %q, want retained %q", runtime.Name, profile.Status.ProjectedRuntimeName)
	}
}

// primaryGPUSpec returns a projectable GPU profile marked as its model's
// primary, used as the model-slug-primary baseline across the Reduced/Both tests.
func primaryGPUSpec() aimv1alpha2.AIMProfileSpecCommon {
	spec := gpuSpec()
	spec.Primary = true
	return spec
}

// autoSelectOn reports whether a runtime's first supportedModelFormat advertises
// autoSelect. Every projected runtime (per-profile and model-slug primary alike)
// keeps it off, so this is used as a regression guard.
func autoSelectOn(formats []kservev1alpha1.SupportedModelFormat) bool {
	return len(formats) > 0 && formats[0].AutoSelect != nil && *formats[0].AutoSelect
}

// TestModelSlugRuntimeName pins the portable, vendor-independent slug: keyed on
// aimId (no accelerator/precision axis), RFC-1123 normalised, aim- prefixed.
func TestModelSlugRuntimeName(t *testing.T) {
	t.Parallel()
	if got, want := serving.ModelSlug("qwen/qwen3-32b"), "qwen-qwen3-32b"; got != want {
		t.Errorf("ModelSlug = %q, want %q", got, want)
	}
	if got, want := serving.ModelSlugRuntimeName("qwen/qwen3-32b"), "aim-qwen-qwen3-32b"; got != want {
		t.Errorf("ModelSlugRuntimeName = %q, want %q", got, want)
	}
}

// TestProfilePlanResources_ReducedModelSlugPrimary pins the Reduced namespace
// projection: a projectable primary AIMProfile yields exactly one model-slug
// ServingRuntime named aim-<model-slug> (NOT aim-<profile.Name>) with autoSelect
// OFF, plus its colocated ConfigMap, routed through the force-apply bucket.
func TestProfilePlanResources_ReducedModelSlugPrimary(t *testing.T) {
	spec := primaryGPUSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "team-a"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	affinity := BuildNodeAffinity(aimv1alpha2.AcceleratorTypeGPU, "MI300X", "unpartitioned")
	obs := ProfileObservation{
		ProfileFetchResult: ProfileFetchResult{modelSlugWinner: true},
		matchResult:        NodeMatchResult{MatchingNodes: 3, NodeAffinity: affinity},
		resolvedResources:  ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:         true,
		projectable:        true,
		yamlContract:       profileyaml.CanonicalContract(&spec),
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)

	force := plan.GetToApplyWithForce()
	sr := findServingRuntime(t, force) // exactly one SR in Reduced mode

	wantName := serving.ModelSlugRuntimeName(spec.AimId)
	if sr.Name != wantName {
		t.Errorf("model-slug runtime name = %q, want %q", sr.Name, wantName)
	}
	if sr.Name == serving.RuntimeName(profile.Name) {
		t.Errorf("Reduced mode must not project the per-profile runtime name %q", sr.Name)
	}
	if autoSelectOn(sr.Spec.SupportedModelFormats) {
		t.Errorf("model-slug primary must have autoSelect=false")
	}
	if sr.Labels[constants.LabelProfile] == "" {
		t.Errorf("model-slug runtime must still correlate to its backing profile: %v", sr.Labels)
	}
	if !hasConfigMap(force, wantName, profile.Namespace) {
		t.Errorf("expected colocated ConfigMap %q in force bucket", wantName)
	}
}

// TestProfilePlanResources_ReducedSkipsNonPrimaryAndNonProjectable pins the
// post-election gates: Reduced publishes nothing for a non-primary profile, and
// nothing for an elected primary that is not projectable.
func TestProfilePlanResources_ReducedSkipsNonPrimaryAndNonProjectable(t *testing.T) {
	cases := map[string]struct {
		primary     bool
		projectable bool
	}{
		"non-primary projectable":     {primary: false, projectable: true},
		"primary not projectable":     {primary: true, projectable: false},
		"non-primary not projectable": {primary: false, projectable: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spec := gpuSpec()
			spec.Primary = tc.primary
			profile := &aimv1alpha2.AIMProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
				Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
			}
			obs := ProfileObservation{
				ProfileFetchResult: ProfileFetchResult{modelSlugWinner: tc.primary},
				deployable:         true,
				projectable:        tc.projectable,
			}
			obs.profile = profile

			r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
			plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)
			if n := countServingRuntimes(plan.GetToApplyWithForce()); n != 0 {
				t.Fatalf("Reduced must publish no runtime, got %d", n)
			}
		})
	}
}

// TestClusterProfilePlanResources_ReducedModelSlugPrimary pins the Reduced
// cluster projection: a projectable primary AIMClusterProfile yields exactly one
// bare model-slug ClusterServingRuntime named aim-<model-slug> with autoSelect
// OFF and no colocated ConfigMap.
func TestClusterProfilePlanResources_ReducedModelSlugPrimary(t *testing.T) {
	spec := primaryGPUSpec()
	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x"},
		Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
	}
	affinity := BuildNodeAffinity(aimv1alpha2.AcceleratorTypeGPU, "MI300X", "unpartitioned")
	obs := ClusterProfileObservation{
		ClusterProfileFetchResult: ClusterProfileFetchResult{modelSlugWinner: true},
		matchResult:               NodeMatchResult{MatchingNodes: 3, NodeAffinity: affinity},
		resolvedResources:         ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:                true,
		projectable:               true,
	}
	obs.profile = profile

	r := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: profile}, obs)

	force := plan.GetToApplyWithForce()
	csr := findClusterServingRuntime(t, force)
	if csr.Name != serving.ModelSlugRuntimeName(spec.AimId) {
		t.Errorf("model-slug runtime name = %q, want %q", csr.Name, serving.ModelSlugRuntimeName(spec.AimId))
	}
	if autoSelectOn(csr.Spec.SupportedModelFormats) {
		t.Errorf("model-slug primary must have autoSelect=false")
	}
	for _, o := range force {
		if _, ok := o.(*corev1.ConfigMap); ok {
			t.Errorf("bare model-slug ClusterServingRuntime must not project a ConfigMap")
		}
	}
}

// TestProfilePlanResources_BothModeNoAmbiguity pins that Both mode publishes the
// per-profile runtime AND the model-slug primary together, with NEITHER
// advertising autoSelect so native KServe auto-selection is never made ambiguous
// (both share the single model format; consumers reference by name).
func TestProfilePlanResources_BothModeNoAmbiguity(t *testing.T) {
	spec := primaryGPUSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "team-a"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	affinity := BuildNodeAffinity(aimv1alpha2.AcceleratorTypeGPU, "MI300X", "unpartitioned")
	obs := ProfileObservation{
		ProfileFetchResult: ProfileFetchResult{modelSlugWinner: true},
		matchResult:        NodeMatchResult{MatchingNodes: 3, NodeAffinity: affinity},
		resolvedResources:  ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:         true,
		projectable:        true,
		yamlContract:       profileyaml.CanonicalContract(&spec),
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeBoth}
	plan := r.PlanResources(context.Background(), controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}, obs)

	force := plan.GetToApplyWithForce()
	if n := countServingRuntimes(force); n != 2 {
		t.Fatalf("Both mode must publish per-profile + model-slug runtimes, got %d", n)
	}

	perProfileName := serving.RuntimeName(profile.Name)
	modelSlugName := serving.ModelSlugRuntimeName(spec.AimId)
	autoSelectCount := 0
	names := map[string]bool{}
	for _, o := range force {
		sr, ok := o.(*kservev1alpha1.ServingRuntime)
		if !ok {
			continue
		}
		names[sr.Name] = true
		if autoSelectOn(sr.Spec.SupportedModelFormats) {
			autoSelectCount++
			t.Errorf("autoSelect is on for %q; every projected runtime must keep it off", sr.Name)
		}
	}
	if !names[perProfileName] || !names[modelSlugName] {
		t.Errorf("Both mode must emit %q and %q, got names %v", perProfileName, modelSlugName, names)
	}
	if autoSelectCount != 0 {
		t.Errorf("no projected runtime may advertise autoSelect, got %d", autoSelectCount)
	}
}

// TestDecorateRuntimeProjection pins the RuntimeProjected condition semantics of
// the pure decorator. wasProjected is the "a runtime was projected before"
// signal (derived by callers from the prior condition, not a live Get).
func TestDecorateRuntimeProjection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		projectable  bool
		wasProjected bool
		nodeErr      error
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantNoCond   bool
	}{
		{
			name:        "projectable -> True",
			projectable: true,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  aimv1alpha2.AIMProfileReasonRuntimeProjected,
		},
		{
			name:         "gate false but was projected before -> Degraded",
			projectable:  false,
			wasProjected: true,
			wantStatus:   metav1.ConditionFalse,
			wantReason:   aimv1alpha2.AIMProfileReasonRuntimeDegraded,
		},
		{
			name:        "gate false, never projected -> no condition",
			projectable: false,
			wantNoCond:  true,
		},
		{
			name:         "transient node error does not flap to Degraded",
			projectable:  false,
			wasProjected: true,
			nodeErr:      fmt.Errorf("connection refused"),
			wantNoCond:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cm := controllerutils.NewConditionManager(nil)
			decorateRuntimeProjection(cm, tc.projectable, tc.wasProjected, tc.nodeErr)
			got := cm.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected)
			if tc.wantNoCond {
				if got != nil {
					t.Fatalf("expected no RuntimeProjected condition, got status=%q reason=%q", got.Status, got.Reason)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected RuntimeProjected condition, none found")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestProfileDecorateStatus_DegradesFromPriorConditionOnGateFlip drives two
// reconcile cycles through the namespace DecorateStatus: a projectable profile
// records RuntimeProjected=True, then a gate flip (nodes vanished) must degrade
// to False/RuntimeDegraded — derived purely from the prior condition the
// ConditionManager is seeded with (mirroring the pipeline), with no runtime
// existence Get.
func TestProfileDecorateStatus_DegradesFromPriorConditionOnGateFlip(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}

	projectable := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 3},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       true,
	}
	projectable.profile = profile

	// Cycle 1: projectable -> True.
	cm1 := controllerutils.NewConditionManager(nil)
	r.DecorateStatus(&aimv1alpha2.AIMProfileStatus{}, cm1, projectable)
	if got := cm1.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected); got == nil ||
		got.Status != metav1.ConditionTrue || got.Reason != aimv1alpha2.AIMProfileReasonRuntimeProjected {
		t.Fatalf("cycle 1: want RuntimeProjected True/%s, got %+v", aimv1alpha2.AIMProfileReasonRuntimeProjected, got)
	}

	// Cycle 2: gate flips false; seed a fresh ConditionManager from cycle-1's
	// conditions (as the pipeline seeds it from existing status) so the prior
	// True is visible without any client call.
	nonProjectable := projectable
	nonProjectable.matchResult = NodeMatchResult{MatchingNodes: 0}
	nonProjectable.projectable = false
	cm2 := controllerutils.NewConditionManager(cm1.Conditions())
	r.DecorateStatus(&aimv1alpha2.AIMProfileStatus{}, cm2, nonProjectable)
	if got := cm2.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected); got == nil ||
		got.Status != metav1.ConditionFalse || got.Reason != aimv1alpha2.AIMProfileReasonRuntimeDegraded {
		t.Fatalf("cycle 2: want RuntimeProjected False/%s, got %+v", aimv1alpha2.AIMProfileReasonRuntimeDegraded, got)
	}
}

// TestProfileDecorateStatus_SilentWhenNeverProjected pins that a namespace
// profile that is not projectable and was never projected records no
// RuntimeProjected condition at all (silent), not a spurious Degraded.
func TestProfileDecorateStatus_SilentWhenNeverProjected(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 0},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       false,
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	cm := controllerutils.NewConditionManager(nil)
	r.DecorateStatus(&aimv1alpha2.AIMProfileStatus{}, cm, obs)
	if got := cm.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected); got != nil {
		t.Fatalf("never-projected profile must stay silent, got %+v", got)
	}
}

// TestClusterProfileDecorateStatus_DegradesFromPriorConditionOnGateFlip is the
// cluster-scoped mirror of the namespace gate-flip degrade test.
func TestClusterProfileDecorateStatus_DegradesFromPriorConditionOnGateFlip(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
	}
	r := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}

	projectable := ClusterProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 3},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       true,
	}
	projectable.profile = profile

	cm1 := controllerutils.NewConditionManager(nil)
	r.DecorateStatus(&aimv1alpha2.AIMProfileStatus{}, cm1, projectable)
	if got := cm1.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected); got == nil ||
		got.Status != metav1.ConditionTrue || got.Reason != aimv1alpha2.AIMProfileReasonRuntimeProjected {
		t.Fatalf("cycle 1: want RuntimeProjected True/%s, got %+v", aimv1alpha2.AIMProfileReasonRuntimeProjected, got)
	}

	nonProjectable := projectable
	nonProjectable.matchResult = NodeMatchResult{MatchingNodes: 0}
	nonProjectable.projectable = false
	cm2 := controllerutils.NewConditionManager(cm1.Conditions())
	r.DecorateStatus(&aimv1alpha2.AIMProfileStatus{}, cm2, nonProjectable)
	if got := cm2.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected); got == nil ||
		got.Status != metav1.ConditionFalse || got.Reason != aimv1alpha2.AIMProfileReasonRuntimeDegraded {
		t.Fatalf("cycle 2: want RuntimeProjected False/%s, got %+v", aimv1alpha2.AIMProfileReasonRuntimeDegraded, got)
	}
}

// TestClusterProfileDecorateStatus_SilentWhenNeverProjected is the
// cluster-scoped mirror of the never-projected silence test.
func TestClusterProfileDecorateStatus_SilentWhenNeverProjected(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ClusterProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 0},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       false,
	}
	obs.profile = profile

	r := &ClusterProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	cm := controllerutils.NewConditionManager(nil)
	r.DecorateStatus(&aimv1alpha2.AIMProfileStatus{}, cm, obs)
	if got := cm.Get(aimv1alpha2.AIMProfileConditionRuntimeProjected); got != nil {
		t.Fatalf("never-projected cluster profile must stay silent, got %+v", got)
	}
}

// TestProfileDecorateStatus_RecordsProjectedRuntimeName pins the discoverability
// field: under a per-profile mode a projectable profile records
// status.projectedRuntimeName = RuntimeName(profile.Name) (the readable lookup
// for the opaque hashed object name), and leaves the model-slug field empty
// (Exhaustive publishes no model-slug primary).
func TestProfileDecorateStatus_RecordsProjectedRuntimeName(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 3},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       true,
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	status := &aimv1alpha2.AIMProfileStatus{}
	r.DecorateStatus(status, controllerutils.NewConditionManager(nil), obs)

	if want := serving.RuntimeName(profile.Name); status.ProjectedRuntimeName != want {
		t.Errorf("projectedRuntimeName = %q, want %q", status.ProjectedRuntimeName, want)
	}
	if status.ProjectedModelSlugRuntimeName != "" {
		t.Errorf("projectedModelSlugRuntimeName = %q, want empty under Exhaustive", status.ProjectedModelSlugRuntimeName)
	}
}

// TestProfileDecorateStatus_ProjectedNameKeptOnGateFlip pins that the recorded
// projectedRuntimeName follows the additive/degrade lifecycle: once written it
// is not cleared when the projection gate later flips but the runtime survives
// (RuntimeProjected=Degraded). Modeled the way the pipeline carries status
// forward — the SAME status object is reused across the two reconcile cycles.
func TestProfileDecorateStatus_ProjectedNameKeptOnGateFlip(t *testing.T) {
	spec := gpuSpec()
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}

	projectable := ProfileObservation{
		matchResult:       NodeMatchResult{MatchingNodes: 3},
		resolvedResources: ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:        true,
		projectable:       true,
	}
	projectable.profile = profile

	status := &aimv1alpha2.AIMProfileStatus{}
	cm1 := controllerutils.NewConditionManager(nil)
	r.DecorateStatus(status, cm1, projectable)
	want := serving.RuntimeName(profile.Name)
	if status.ProjectedRuntimeName != want {
		t.Fatalf("cycle 1: projectedRuntimeName = %q, want %q", status.ProjectedRuntimeName, want)
	}

	// Cycle 2: gate flips false, reusing the carried-forward status. The name
	// must survive (degrade keeps the runtime, so its discoverable name stays).
	nonProjectable := projectable
	nonProjectable.matchResult = NodeMatchResult{MatchingNodes: 0}
	nonProjectable.projectable = false
	cm2 := controllerutils.NewConditionManager(cm1.Conditions())
	r.DecorateStatus(status, cm2, nonProjectable)
	if status.ProjectedRuntimeName != want {
		t.Errorf("cycle 2: projectedRuntimeName = %q, want it kept as %q on gate flip", status.ProjectedRuntimeName, want)
	}
}

// TestProfileDecorateStatus_ReducedRecordsModelSlugName pins that under Reduced
// mode a projectable model primary records status.projectedModelSlugRuntimeName
// (the readable aim-<model-slug>) and leaves the per-profile field empty (Reduced
// projects no per-profile runtime).
func TestProfileDecorateStatus_ReducedRecordsModelSlugName(t *testing.T) {
	spec := gpuSpec()
	spec.Primary = true
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		ProfileFetchResult: ProfileFetchResult{modelSlugWinner: true},
		matchResult:        NodeMatchResult{MatchingNodes: 3},
		resolvedResources:  ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:         true,
		projectable:        true,
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
	status := &aimv1alpha2.AIMProfileStatus{}
	r.DecorateStatus(status, controllerutils.NewConditionManager(nil), obs)

	if want := serving.ModelSlugRuntimeName(spec.AimId); status.ProjectedModelSlugRuntimeName != want {
		t.Errorf("projectedModelSlugRuntimeName = %q, want %q", status.ProjectedModelSlugRuntimeName, want)
	}
	if status.ProjectedRuntimeName != "" {
		t.Errorf("projectedRuntimeName = %q, want empty under Reduced", status.ProjectedRuntimeName)
	}
}

// TestProfileDecorateStatus_BothRecordsBothNames pins that under Both mode a
// projectable model primary records both the per-profile and model-slug runtime
// names.
func TestProfileDecorateStatus_BothRecordsBothNames(t *testing.T) {
	spec := gpuSpec()
	spec.Primary = true
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-mi300x", Namespace: "ns"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		ProfileFetchResult: ProfileFetchResult{modelSlugWinner: true},
		matchResult:        NodeMatchResult{MatchingNodes: 3},
		resolvedResources:  ResolveResources(spec.AcceleratorType, spec.AcceleratorCount, nil, spec.AcceleratorModel, nil),
		deployable:         true,
		projectable:        true,
	}
	obs.profile = profile

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeBoth}
	status := &aimv1alpha2.AIMProfileStatus{}
	r.DecorateStatus(status, controllerutils.NewConditionManager(nil), obs)

	if want := serving.RuntimeName(profile.Name); status.ProjectedRuntimeName != want {
		t.Errorf("projectedRuntimeName = %q, want %q", status.ProjectedRuntimeName, want)
	}
	if want := serving.ModelSlugRuntimeName(spec.AimId); status.ProjectedModelSlugRuntimeName != want {
		t.Errorf("projectedModelSlugRuntimeName = %q, want %q", status.ProjectedModelSlugRuntimeName, want)
	}
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func hasConfigMap(objs []client.Object, name, namespace string) bool {
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == name && cm.Namespace == namespace {
			return true
		}
	}
	return false
}
