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

package aimservice

import (
	"context"
	"errors"
	"testing"
	"time"

	servingv1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
	v1alpha1service "github.com/amd-enterprise-ai/aim-engine/internal/v1alpha1/aimservice"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

const (
	testProfileA           = "profile-a"
	testServiceName        = "svc"
	testModelIDFP8         = "qwen/qwen3-32b-fp8"
	testClusterProfileName = "cluster-profile"

	componentNameInferenceService = "InferenceService"
	componentNameHTTPRoute        = "HTTPRoute"
	componentNameProfileCache     = "ProfileCache"
)

func TestGenerateProfileCacheName_SharedDeterministic(t *testing.T) {
	a, err := GenerateProfileCacheName("my-profile", "ns-alpha", "svc-a", "uid-a", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := GenerateProfileCacheName("my-profile", "ns-alpha", "svc-b", "uid-b", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a != b {
		t.Fatalf("Shared cache name must depend only on (profile, namespace, scope) so services in the same namespace converge: %q vs %q", a, b)
	}
}

func TestGenerateProfileCacheName_SharedNamespaceScoped(t *testing.T) {
	same, _ := GenerateProfileCacheName("profile-x", "ns-a", "", "", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	other, _ := GenerateProfileCacheName("profile-x", "ns-b", "", "", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	if same == other {
		t.Fatalf("Shared cache names must differ across namespaces")
	}
}

// TestGenerateProfileCacheName_SharedScopeIsolation guards against the
// pre-fix collision where a namespace AIMProfile and a cluster
// AIMClusterProfile with the same name in the same service namespace shared
// a single AIMProfileCache and could service the wrong workload.
func TestGenerateProfileCacheName_SharedScopeIsolation(t *testing.T) {
	ns, _ := GenerateProfileCacheName("collides", "ns-alpha", "", "", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	cluster, _ := GenerateProfileCacheName("collides", "ns-alpha", "", "", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeCluster)
	if ns == cluster {
		t.Fatalf("Shared cache names must differ across profile scopes for the same profile name + namespace: %q == %q", ns, cluster)
	}
}

func TestGenerateProfileCacheName_DedicatedPerService(t *testing.T) {
	a, err := GenerateProfileCacheName("my-profile", "ns-alpha", "svc-a", "uid-a", aimv1alpha1.CachingModeDedicated, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := GenerateProfileCacheName("my-profile", "ns-alpha", "svc-b", "uid-b", aimv1alpha1.CachingModeDedicated, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a == b {
		t.Fatalf("Dedicated cache names must differ across services on the same profile: %q vs %q", a, b)
	}
}

func TestGenerateProfileCacheName_DedicatedSurvivesRecreate(t *testing.T) {
	// Same service name but different UIDs (delete-and-recreate) must
	// produce different cache names so the new instance does not pick up
	// the old cache before the previous one is GC'd.
	a, err := GenerateProfileCacheName("my-profile", "ns-alpha", "svc", "uid-old", aimv1alpha1.CachingModeDedicated, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := GenerateProfileCacheName("my-profile", "ns-alpha", "svc", "uid-new", aimv1alpha1.CachingModeDedicated, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a == b {
		t.Fatalf("Dedicated cache names must include service UID to survive delete-and-recreate: %q vs %q", a, b)
	}
}

func TestGenerateProfileCacheName_SharedAndDedicatedDiffer(t *testing.T) {
	shared, _ := GenerateProfileCacheName("my-profile", "ns-alpha", "svc", "uid", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	dedicated, _ := GenerateProfileCacheName("my-profile", "ns-alpha", "svc", "uid", aimv1alpha1.CachingModeDedicated, aimv1alpha1.AIMResolutionScopeNamespace)
	if shared == dedicated {
		t.Fatalf("Shared and Dedicated caches must never collide: %q == %q", shared, dedicated)
	}
}

// Regression test: distinct long profile names that share a prefix must not
// collide on the same cache resource after truncation.
func TestGenerateProfileCacheName_NoTruncationCollision(t *testing.T) {
	namespace := "qa-test-may12"
	profileA := "amdenterpriseai-aim-meta-llama-llama-3-1x-mi300x-thr-fp16-e87a"
	profileB := "amdenterpriseai-aim-meta-llama-llama-3-1x-mi300x-thr-fp16-db70"

	nameA, err := GenerateProfileCacheName(profileA, namespace, "", "", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate A: %v", err)
	}
	nameB, err := GenerateProfileCacheName(profileB, namespace, "", "", aimv1alpha1.CachingModeShared, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}

	if nameA == nameB {
		t.Fatalf("profile cache names collided for distinct profiles: %q", nameA)
	}
}

func TestComposeState_NoProfile(t *testing.T) {
	r := &ProfileServiceReconciler{}
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	fetch := ServiceFetchResult{service: service}
	obs := r.ComposeState(context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha1.AIMService]{Object: service},
		fetch,
	)

	if obs.resolvedProfileSpec != nil {
		t.Errorf("expected no resolved profile spec when no profile fetched")
	}
	if obs.hasModelSources {
		t.Errorf("hasModelSources should be false when no profile fetched")
	}
	if obs.profileCacheReady {
		t.Errorf("profileCacheReady should be false when no profile fetched")
	}
}

func TestComposeState_NamespaceProfileResolved(t *testing.T) {
	r := &ProfileServiceReconciler{}
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: testProfileA, Namespace: "ns"},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: *sampleProfileSpec(),
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
	}
	fetch := ServiceFetchResult{
		service: service,
		profile: controllerutils.FetchResult[*aimv1alpha2.AIMProfile]{Value: profile},
	}
	obs := r.ComposeState(context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha1.AIMService]{Object: service},
		fetch,
	)

	if obs.profileName != testProfileA {
		t.Errorf("profile name not resolved: %q", obs.profileName)
	}
	if obs.profileScope != aimv1alpha1.AIMResolutionScopeNamespace {
		t.Errorf("profile scope should be namespace: %v", obs.profileScope)
	}
	if obs.resolvedProfileSpec == nil {
		t.Errorf("resolved profile spec should be populated")
	}
}

func TestComposeState_ClusterProfileFallback(t *testing.T) {
	r := &ProfileServiceReconciler{}
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	clusterProfile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterProfileName},
		Spec: aimv1alpha2.AIMClusterProfileSpec{
			AIMProfileSpecCommon: *sampleProfileSpec(),
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
	}
	fetch := ServiceFetchResult{
		service:        service,
		clusterProfile: controllerutils.FetchResult[*aimv1alpha2.AIMClusterProfile]{Value: clusterProfile},
	}
	obs := r.ComposeState(context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha1.AIMService]{Object: service},
		fetch,
	)
	if obs.profileScope != aimv1alpha1.AIMResolutionScopeCluster {
		t.Errorf("profile scope should be cluster: %v", obs.profileScope)
	}
	if obs.profileName != testClusterProfileName {
		t.Errorf("cluster profile name not resolved: %q", obs.profileName)
	}
}

// TestBuildInferenceServiceFromProfile_ReferencesRuntime pins the Phase C
// contract: the ISVC references the projected runtime (aim-<profile.Name>) via
// predictor.model.runtime + modelFormat and emits NO inline predictor (no
// container image, no profile ConfigMap volume/mount, no framework env). The
// runtime carries all of that; the service only references it.
func TestBuildInferenceServiceFromProfile_ReferencesRuntime(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
		},
	}
	profileSpec := sampleProfileSpec()
	profileStatus := &aimv1alpha2.AIMProfileStatus{
		Status: constants.AIMStatusReady,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
		},
		// Affinity belongs on the runtime, never the overlay; pin that the
		// overlay drops it even when the profile resolved one.
		ResolvedNodeAffinity: &corev1.NodeAffinity{},
	}

	r := &ProfileServiceReconciler{}
	obs := ServiceObservation{
		ServiceFetchResult:    ServiceFetchResult{service: service},
		resolvedProfileSpec:   profileSpec,
		resolvedProfileStatus: profileStatus,
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
	}
	r.composeDerivedNames(context.Background(), &obs)
	if obs.configErr != nil {
		t.Fatalf("composeDerivedNames returned error: %v", obs.configErr)
	}

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("buildInferenceServiceFromProfile returned nil")
	}

	if isvc.Labels[constants.LabelService] != testServiceName {
		t.Errorf("missing service label: %v", isvc.Labels)
	}
	if isvc.Labels[constants.LabelProfile] != testProfileA {
		t.Errorf("missing profile label: %v", isvc.Labels)
	}

	model := isvc.Spec.Predictor.Model
	if model == nil {
		t.Fatalf("expected predictor.model to be set")
	}
	if model.Runtime == nil || *model.Runtime != serving.RuntimeName(testProfileA) {
		t.Errorf("predictor.model.runtime = %v, want %q", model.Runtime, serving.RuntimeName(testProfileA))
	}
	if model.ModelFormat.Name != serving.RuntimeModelFormat {
		t.Errorf("modelFormat.name = %q, want %q", model.ModelFormat.Name, serving.RuntimeModelFormat)
	}

	// No inline predictor: no explicit container, no inline image, no profile
	// ConfigMap volume, no framework env on the overlay.
	if len(isvc.Spec.Predictor.Containers) != 0 {
		t.Errorf("overlay must not inline predictor containers, got %d", len(isvc.Spec.Predictor.Containers))
	}
	if model.Image != "" {
		t.Errorf("overlay model must not carry an inline image, got %q", model.Image)
	}
	for _, v := range isvc.Spec.Predictor.Volumes {
		if v.Name == profileVolumePrefix {
			t.Errorf("overlay must not mount the profile ConfigMap volume %q (runtime owns it)", profileVolumePrefix)
		}
	}
	for _, e := range model.Env {
		if e.Name == constants.EnvAIMProfileID {
			t.Errorf("overlay must not carry framework env %q (runtime owns it)", constants.EnvAIMProfileID)
		}
	}
	if isvc.Spec.Predictor.Affinity != nil {
		t.Errorf("overlay must not carry node affinity (runtime owns it)")
	}
	// No service-level resource override -> the overlay leaves resources to the
	// runtime (empty requests/limits on the model).
	if len(model.Resources.Requests) != 0 || len(model.Resources.Limits) != 0 {
		t.Errorf("overlay must not carry profile resources without a service override, got %+v", model.Resources)
	}
}

func TestBuildInferenceServiceFromProfile_ServiceAccountPrecedence(t *testing.T) {
	profileSpec := sampleProfileSpec()
	profileSpec.ServiceAccountName = "profile-sa"
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	obs := ServiceObservation{
		resolvedProfileSpec:   profileSpec,
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		isvcName:              "service-isvc",
	}

	isvc := buildInferenceServiceFromProfile(service, obs)
	if got := isvc.Spec.Predictor.ServiceAccountName; got != "profile-sa" {
		t.Fatalf("profile service account fallback = %q, want profile-sa", got)
	}

	service.Spec.ServiceAccountName = "service-sa"
	isvc = buildInferenceServiceFromProfile(service, obs)
	if got := isvc.Spec.Predictor.ServiceAccountName; got != "service-sa" {
		t.Fatalf("service account override = %q, want service-sa", got)
	}
}

// TestBuildInferenceServiceFromProfile_StickyReference pins that an existing
// ISVC keeps its current runtime reference even when the resolved profile name
// would now project a different runtime (e.g. a projection-mode change), so a
// mode change never re-rolls a live ISVC.
func TestBuildInferenceServiceFromProfile_StickyReference(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
		},
	}
	existingRef := "aim-some-previous-runtime"
	existing := &servingv1beta1.InferenceService{
		Spec: servingv1beta1.InferenceServiceSpec{
			Predictor: servingv1beta1.PredictorSpec{
				Model: &servingv1beta1.ModelSpec{Runtime: ptr.To(existingRef)},
			},
		},
	}

	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:          service,
			inferenceService: controllerutils.FetchResult[*servingv1beta1.InferenceService]{Value: existing},
		},
		resolvedProfileSpec:   sampleProfileSpec(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
	}
	(&ProfileServiceReconciler{}).composeDerivedNames(context.Background(), &obs)

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("expected non-nil ISVC")
	}
	if got := isvc.Spec.Predictor.Model.Runtime; got == nil || *got != existingRef {
		t.Errorf("runtime reference must be sticky: got %v, want %q", got, existingRef)
	}
}

// TestBuildInferenceServiceFromProfile_StampsRuntimeProfile pins the lazy
// runtime-projection fast-path: a service stamps the backing profile name so the
// lazy watcher can materialize the runtime in the service's namespace before it
// exists. Stamped for BOTH scopes — a cluster profile is the cross-scope case,
// and a namespace profile needs it under Reduced mode (no eager per-profile
// runtime), where the watcher resolves the namespace AIMProfile from it.
func TestBuildInferenceServiceFromProfile_StampsRuntimeProfile(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testClusterProfileName},
		},
	}
	obs := ServiceObservation{
		ServiceFetchResult:    ServiceFetchResult{service: service},
		resolvedProfileSpec:   sampleProfileSpec(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testClusterProfileName,
		profileScope:          aimv1alpha1.AIMResolutionScopeCluster,
	}
	(&ProfileServiceReconciler{}).composeDerivedNames(context.Background(), &obs)

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("expected non-nil ISVC")
	}
	if got := isvc.Annotations[constants.AnnotationRuntimeProfile]; got != testClusterProfileName {
		t.Errorf("cluster-scoped service must stamp runtime-profile annotation, got %q", got)
	}

	// Namespace scope stamps it too so a Reduced-mode namespace-profile-backed
	// service can be lazily completed.
	obs.profileScope = aimv1alpha1.AIMResolutionScopeNamespace
	nsISVC := buildInferenceServiceFromProfile(service, obs)
	if got := nsISVC.Annotations[constants.AnnotationRuntimeProfile]; got != testClusterProfileName {
		t.Errorf("namespace-scoped service must also stamp the runtime-profile annotation, got %q", got)
	}
}

// TestBuildInferenceServiceFromProfile_ServiceResourceOverrideOnModel pins that
// a service-level resource override is the one resource knob carried by the
// overlay (KServe merges it over the runtime container by name).
func TestBuildInferenceServiceFromProfile_ServiceResourceOverrideOnModel(t *testing.T) {
	override := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Gi")},
	}
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile:   &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			Resources: override,
		},
	}
	obs := ServiceObservation{
		ServiceFetchResult:    ServiceFetchResult{service: service},
		resolvedProfileSpec:   sampleProfileSpec(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
	}
	(&ProfileServiceReconciler{}).composeDerivedNames(context.Background(), &obs)

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("expected non-nil ISVC")
	}
	if got := isvc.Spec.Predictor.Model.Resources.Requests.Memory(); got.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("service resource override not carried on model: got %v, want 16Gi", got)
	}
}

func TestBuildInferenceServiceFromProfile_NilSpecReturnsNil(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{service: service},
	}
	if isvc := buildInferenceServiceFromProfile(service, obs); isvc != nil {
		t.Fatalf("expected nil ISVC when profile spec is nil")
	}
}

// TestResolvedModelIdFromProfile verifies the model-id identity follows the
// runtime resolution chain `profile.model_id or config.model_id or config.aim_id`:
// ModelId wins, then modelSources[0].modelId, then aimId.
func TestResolvedModelIdFromProfile(t *testing.T) {
	// ModelId wins even when modelSources carry a different id (matches the
	// runtime, which writes profile.model_id from spec.ModelId).
	modelIDWins := sampleProfileSpec() // ModelId = qwen/qwen3-32b-fp8
	modelIDWins.ModelSources = []aimv1alpha1.AIMModelSource{
		{ModelID: "acme/my-finetune-v1", SourceURI: "hf://acme/my-finetune-v1"},
	}
	if got := resolvedModelId(modelIDWins); got != testModelIDFP8 {
		t.Errorf("ModelId set: resolvedModelId() = %q, want ModelId %q", got, testModelIDFP8)
	}

	// ModelId empty falls back to modelSources[0].modelId.
	viaSources := sampleProfileSpec()
	viaSources.ModelId = ""
	viaSources.ModelSources = []aimv1alpha1.AIMModelSource{
		{ModelID: "acme/my-finetune-v1", SourceURI: "hf://acme/my-finetune-v1"},
	}
	if got := resolvedModelId(viaSources); got != "acme/my-finetune-v1" {
		t.Errorf("ModelId empty + modelSources: resolvedModelId() = %q, want %q", got, "acme/my-finetune-v1")
	}

	// ModelId and modelSources empty falls back to aimId.
	viaAimID := sampleProfileSpec() // AimId = qwen/qwen3-32b
	viaAimID.ModelId = ""
	if got := resolvedModelId(viaAimID); got != "qwen/qwen3-32b" {
		t.Errorf("ModelId + modelSources empty: resolvedModelId() = %q, want aimId %q", got, "qwen/qwen3-32b")
	}
}

// TestBuildInferenceServiceFromProfile_AnnotationPropagation verifies cluster-auth
// annotations propagate from the AIMService to the InferenceService, unrelated
// annotations (including our own control annotations) are dropped, and the
// controller-owned model-id annotation is stamped from the resolved profile
// rather than any user-supplied value.
func TestBuildInferenceServiceFromProfile_AnnotationPropagation(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testServiceName,
			Namespace: "ns",
			Annotations: map[string]string{
				"cluster-auth/allowed-group":            "ce0c754f-bb1b-63bb-5134-5501142effe7",
				"aim.eai.amd.com/model-id":              "user-tried-to-override",
				"aim.eai.amd.com/reconciliation-paused": "true",
				"example.com/foreign":                   "drop-me",
			},
		},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
		},
	}

	profileSpec := sampleProfileSpec()
	profileSpec.ModelSources = []aimv1alpha1.AIMModelSource{
		{ModelID: testModelIDFP8, SourceURI: "hf://qwen/qwen3-32b-fp8"},
	}
	profileStatus := &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady}

	r := &ProfileServiceReconciler{}
	obs := ServiceObservation{
		ServiceFetchResult:    ServiceFetchResult{service: service},
		resolvedProfileSpec:   profileSpec,
		resolvedProfileStatus: profileStatus,
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
	}
	r.composeDerivedNames(context.Background(), &obs)
	if obs.configErr != nil {
		t.Fatalf("composeDerivedNames returned error: %v", obs.configErr)
	}

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("buildInferenceServiceFromProfile returned nil")
	}

	ann := isvc.Annotations
	if got := ann["cluster-auth/allowed-group"]; got != "ce0c754f-bb1b-63bb-5134-5501142effe7" {
		t.Errorf("expected cluster-auth annotation propagated, got %q", got)
	}
	if _, ok := ann["example.com/foreign"]; ok {
		t.Error("foreign annotation example.com/foreign must not be propagated")
	}
	if _, ok := ann["aim.eai.amd.com/reconciliation-paused"]; ok {
		t.Error("control annotation aim.eai.amd.com/reconciliation-paused must not be propagated")
	}
	if got := ann[constants.AnnotationModelId]; got != testModelIDFP8 {
		t.Errorf("model-id annotation = %q, want controller-owned %q (must not be overridable from service spec)", got, testModelIDFP8)
	}
}

func TestGetConfigHealth_NoErrorReturnsEmpty(t *testing.T) {
	obs := ServiceObservation{}
	if got := obs.getConfigHealth(); got.Component != "" {
		t.Errorf("expected empty ComponentHealth when configErr is nil, got %+v", got)
	}
}

func TestGetConfigHealth_SurfacesInvalidSpec(t *testing.T) {
	obs := ServiceObservation{
		configErr: errors.New("assemble profile YAML: invalid engineArgs"),
	}
	health := obs.getConfigHealth()

	if health.Component != "ProfileConfig" {
		t.Errorf("unexpected component: %q", health.Component)
	}
	if health.State != constants.AIMStatusFailed {
		t.Errorf("expected Failed state, got %q", health.State)
	}
	if health.DependencyType != controllerutils.DependencyTypeUpstream {
		t.Errorf("expected upstream dependency type, got %q", health.DependencyType)
	}
	if len(health.Errors) != 1 {
		t.Fatalf("expected a single wrapped error, got %d", len(health.Errors))
	}
	if categorized := controllerutils.CategorizeError(health.Errors[0]); categorized.Category() != controllerutils.ErrorCategoryInvalidSpec {
		t.Errorf("expected InvalidSpec category, got %v", categorized.Category())
	}
}

func TestGetComponentHealth_IncludesConfigAndRouteEntries(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{service: service},
		configErr:          errors.New("bad profile"),
	}
	entries := obs.GetComponentHealth(context.Background(), nil)
	var haveConfig bool
	for _, e := range entries {
		if e.Component == "ProfileConfig" {
			haveConfig = true
		}
		if e.Component == componentNameHTTPRoute {
			t.Errorf("HTTPRoute entry should be suppressed when routing is disabled")
		}
	}
	if !haveConfig {
		t.Errorf("expected ProfileConfig entry to be emitted when configErr is set")
	}
}

// TestGetComponentHealth_ScaleToZeroRequiresRouting verifies the profile
// pipeline surfaces the invalid scale-from-zero-without-routing combination as
// a ConfigValid-driving InvalidSpec error, even before a profile resolves.
func TestGetComponentHealth_ScaleToZeroRequiresRouting(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec:       aimv1alpha1.AIMServiceSpec{MinReplicas: ptr.To(int32(0))},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{service: service},
	}

	entries := obs.GetComponentHealth(context.Background(), nil)

	var cfg *controllerutils.ComponentHealth
	for i := range entries {
		if entries[i].Component == v1alpha1service.ComponentScaleToZeroConfig {
			cfg = &entries[i]
		}
	}
	if cfg == nil {
		t.Fatalf("expected a ScaleToZeroConfig component health entry")
	}
	if cfg.State != constants.AIMStatusFailed {
		t.Errorf("ScaleToZeroConfig state = %q, want Failed", cfg.State)
	}
	if cfg.Reason != aimv1alpha1.AIMServiceReasonRoutingRequired {
		t.Errorf("ScaleToZeroConfig reason = %q, want %q", cfg.Reason, aimv1alpha1.AIMServiceReasonRoutingRequired)
	}
	if len(cfg.Errors) != 1 ||
		controllerutils.CategorizeError(cfg.Errors[0]).Category() != controllerutils.ErrorCategoryInvalidSpec {
		t.Errorf("expected a single InvalidSpec error, got %+v", cfg.Errors)
	}
}

// TestGetComponentHealth_MultiListenerRequiresHostname verifies the profile
// pipeline surfaces the host-pinning guard (multi-listener parent gateway with
// no routing hostnames) as a ConfigValid-driving InvalidSpec error on the
// HTTPRoute component, sharing the v1alpha1 helper.
func TestGetComponentHealth_MultiListenerRequiresHostname(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	service.Spec.Routing = &aimv1alpha1.AIMRuntimeRoutingConfig{
		Enabled:    ptr.To(true),
		GatewayRef: &gatewayapiv1.ParentReference{Name: "gw"},
	}
	gateway := &gatewayapiv1.Gateway{
		Spec: gatewayapiv1.GatewaySpec{
			Listeners: []gatewayapiv1.Listener{
				{Name: "a", Protocol: gatewayapiv1.HTTPProtocolType, Port: 80},
				{Name: "b", Protocol: gatewayapiv1.HTTPProtocolType, Port: 80},
			},
		},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service: service,
			gateway: controllerutils.FetchResult[*gatewayapiv1.Gateway]{Value: gateway},
		},
	}

	entries := obs.GetComponentHealth(context.Background(), nil)

	var cfg *controllerutils.ComponentHealth
	for i := range entries {
		if entries[i].Component == v1alpha1service.ComponentRouteConfig {
			cfg = &entries[i]
		}
	}
	if cfg == nil {
		t.Fatalf("expected a RouteConfig component health entry")
	}
	if cfg.State != constants.AIMStatusFailed {
		t.Errorf("RouteConfig state = %q, want Failed", cfg.State)
	}
	if cfg.Reason != v1alpha1service.ReasonRouteHostnameRequired {
		t.Errorf("RouteConfig reason = %q, want %q", cfg.Reason, v1alpha1service.ReasonRouteHostnameRequired)
	}
	if len(cfg.Errors) != 1 ||
		controllerutils.CategorizeError(cfg.Errors[0]).Category() != controllerutils.ErrorCategoryInvalidSpec {
		t.Errorf("expected a single InvalidSpec error, got %+v", cfg.Errors)
	}
}

// TestGetComponentHealth_ProfileNotFound_SuppressesDownstream pins F13 part 1:
// when no profile resolves, the planner intentionally skips creating ISVC
// and HTTPRoute, so reporting them as "Creating"/"not found" would mislead
// users. Only the gating Profile (and RuntimeConfig if observed) component
// should be reported.
func TestGetComponentHealth_ProfileNotFound_SuppressesDownstream(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "missing-isvc")
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:          service,
			inferenceService: controllerutils.FetchResult[*servingv1beta1.InferenceService]{Error: notFound},
		},
	}

	entries := obs.GetComponentHealth(context.Background(), nil)

	var profileHealth *controllerutils.ComponentHealth
	for i, e := range entries {
		switch e.Component {
		case "Profile":
			profileHealth = &entries[i]
		case componentNameInferenceService, componentNameHTTPRoute, componentNameProfileCache:
			t.Errorf("downstream component %q should be suppressed when no profile resolved; got %+v", e.Component, e)
		}
	}
	if profileHealth == nil {
		t.Fatalf("expected a Profile component health entry")
	}
	if profileHealth.State != constants.AIMStatusFailed {
		t.Errorf("ProfileNotFound state = %q, want Failed (so the framework rolls it up to Ready=False/ProfileNotFound)", profileHealth.State)
	}
	if profileHealth.Reason != aimv1alpha1.AIMServiceReasonProfileNotFound {
		t.Errorf("ProfileNotFound reason = %q, want %q", profileHealth.Reason, aimv1alpha1.AIMServiceReasonProfileNotFound)
	}
}

// TestGetComponentHealth_BaseProfile_SuppressesDownstream pins F13 part 1
// for the base-profile path: the resolved profile exists but is missing
// aimId or modelSources. The planner skips downstream creation and the user
// must fix the profile before anything can progress.
func TestGetComponentHealth_BaseProfile_SuppressesDownstream(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	baseProfileSpec := &aimv1alpha2.AIMProfileSpecCommon{
		Image:            "registry.example.com/base:0.1",
		AcceleratorModel: "EPYC_ZEN5",
		AcceleratorType:  "cpu",
		AcceleratorCount: 128,
	}
	baseProfileStatus := &aimv1alpha2.AIMProfileStatus{
		Status:     constants.AIMStatusReady,
		Deployable: false,
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "missing-isvc")
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:          service,
			inferenceService: controllerutils.FetchResult[*servingv1beta1.InferenceService]{Error: notFound},
		},
		profileName:           "base-profile",
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
		resolvedProfileSpec:   baseProfileSpec,
		resolvedProfileStatus: baseProfileStatus,
	}

	entries := obs.GetComponentHealth(context.Background(), nil)

	var profileHealth *controllerutils.ComponentHealth
	for i, e := range entries {
		switch e.Component {
		case "Profile":
			profileHealth = &entries[i]
		case componentNameInferenceService, componentNameHTTPRoute, componentNameProfileCache:
			t.Errorf("downstream component %q should be suppressed when profile is a base profile; got %+v", e.Component, e)
		}
	}
	if profileHealth == nil {
		t.Fatalf("expected a Profile component health entry")
	}
	if profileHealth.State != constants.AIMStatusFailed {
		t.Errorf("base profile state = %q, want Failed", profileHealth.State)
	}
	if profileHealth.Reason != aimv1alpha1.AIMServiceReasonBaseProfile {
		t.Errorf("base profile reason = %q, want %q", profileHealth.Reason, aimv1alpha1.AIMServiceReasonBaseProfile)
	}
}

// TestGetComponentHealth_ResolvedProfile_KeepsDownstream confirms the
// suppression in F13 only fires for terminal-by-default Profile failures
// (base profile / not-found). When the profile resolves to a real spec —
// even if it's still progressing — downstream conditions remain visible
// so users can watch cache / ISVC progress.
func TestGetComponentHealth_ResolvedProfile_KeepsDownstream(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{}, "missing-isvc")
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:          service,
			inferenceService: controllerutils.FetchResult[*servingv1beta1.InferenceService]{Error: notFound},
		},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
		resolvedProfileSpec:   sampleProfileSpec(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady, Deployable: true},
	}

	entries := obs.GetComponentHealth(context.Background(), nil)

	var sawISVC bool
	for _, e := range entries {
		if e.Component == componentNameInferenceService {
			sawISVC = true
		}
	}
	if !sawISVC {
		t.Errorf("expected InferenceService entry for a deployable resolved profile; got %+v", entries)
	}
}

// TestGetComponentHealth_ScaleToZeroIdleReportsReady pins the v1alpha2 fix for
// the idle-readiness gap: a healthily-idled scale-to-zero service (KEDA scaled
// the predictor to zero, no pods, HPA reporting ScalingDisabled) must report
// the InferenceServicePods and HPA components as Ready/ScaledToZero rather than
// stalling at "no pods"/"waiting" forever.
func TestGetComponentHealth_ScaleToZeroIdleReportsReady(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			MinReplicas: ptr.To(int32(0)),
			MaxReplicas: ptr.To(int32(3)),
		},
	}
	// Routing enabled (promoted field from the inlined runtime config) so the
	// scale-to-zero routing prerequisite passes and does not add an unrelated
	// ConfigValid=False entry.
	service.Spec.Routing = &aimv1alpha1.AIMRuntimeRoutingConfig{Enabled: ptr.To(true)}

	// HPA at the idle floor: ScalingActive=False with KEDA's authoritative
	// ScalingDisabled reason.
	idleHPA := &autoscalingv2.HorizontalPodAutoscaler{
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
				{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: "ScalingDisabled"},
			},
		},
	}
	emptyPods := &controllerutils.FetchResult[*corev1.PodList]{Value: &corev1.PodList{}}

	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:              service,
			inferenceService:     controllerutils.FetchResult[*servingv1beta1.InferenceService]{Value: &servingv1beta1.InferenceService{}},
			inferenceServicePods: emptyPods,
			hpa:                  controllerutils.FetchResult[*autoscalingv2.HorizontalPodAutoscaler]{Value: idleHPA},
		},
		resolvedProfileSpec:   sampleProfileSpec(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady, Deployable: true},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
	}

	entries := obs.GetComponentHealth(context.Background(), nil)

	var pods, hpa *controllerutils.ComponentHealth
	for i := range entries {
		switch entries[i].Component {
		case "InferenceServicePods":
			pods = &entries[i]
		case "HPA":
			hpa = &entries[i]
		}
	}

	if pods == nil {
		t.Fatalf("expected an InferenceServicePods entry; got %+v", entries)
	}
	if pods.State != constants.AIMStatusReady || pods.Reason != aimv1alpha1.AIMServiceReasonScaledToZero {
		t.Errorf("InferenceServicePods = %q/%q, want Ready/ScaledToZero", pods.State, pods.Reason)
	}

	if hpa == nil {
		t.Fatalf("expected an HPA entry; got %+v", entries)
	}
	if hpa.State != constants.AIMStatusReady || hpa.Reason != aimv1alpha1.AIMServiceReasonScaledToZero {
		t.Errorf("HPA = %q/%q, want Ready/ScaledToZero", hpa.State, hpa.Reason)
	}
}

func TestPlanProfileCache_SkipsWhenExisting(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
		},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:      service,
			profileCache: controllerutils.FetchResult[*aimv1alpha2.AIMProfileCache]{Value: &aimv1alpha2.AIMProfileCache{}},
		},
		profileName:         testProfileA,
		resolvedProfileSpec: sampleProfileSpec(),
	}
	if got := planProfileCache(service, obs); got != nil {
		t.Fatalf("planProfileCache should return nil when cache already exists, got %+v", got)
	}
}

// buildTestISVC is a small helper that returns a freshly-built ISVC for a
// service spec with a ready profile. It keeps the autoscaling tests short
// and focused on the one dimension each case is probing.
func buildTestISVC(t *testing.T, spec aimv1alpha1.AIMServiceSpec) *servingv1beta1.InferenceService {
	t.Helper()
	spec.Profile = &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA}

	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec:       spec,
	}
	obs := ServiceObservation{
		ServiceFetchResult:    ServiceFetchResult{service: service},
		resolvedProfileSpec:   sampleProfileSpec(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
	}
	(&ProfileServiceReconciler{}).composeDerivedNames(context.Background(), &obs)
	if obs.configErr != nil {
		t.Fatalf("composeDerivedNames error: %v", obs.configErr)
	}
	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("expected non-nil ISVC")
	}
	return isvc
}

// TestBuildInferenceServiceFromProfile_Replicas_DefaultsToOne guards against
// the v1alpha2 builder silently dropping MaxReplicas (previously it only set
// MinReplicas, leaving MaxReplicas=0 which KServe interprets as "no cap").
func TestBuildInferenceServiceFromProfile_Replicas_DefaultsToOne(t *testing.T) {
	isvc := buildTestISVC(t, aimv1alpha1.AIMServiceSpec{})

	if got := isvc.Spec.Predictor.MinReplicas; got == nil || *got != 1 {
		t.Errorf("MinReplicas: want *1, got %v", got)
	}
	if got := isvc.Spec.Predictor.MaxReplicas; got != 1 {
		t.Errorf("MaxReplicas: want 1, got %d", got)
	}
	if got := isvc.Annotations[constants.AnnotationKServeAutoscalerClass]; got != constants.AutoscalerClassNone {
		t.Errorf("autoscaler class: want %q, got %q", constants.AutoscalerClassNone, got)
	}
}

func TestBuildInferenceServiceFromProfile_Replicas_FixedDisablesHPA(t *testing.T) {
	isvc := buildTestISVC(t, aimv1alpha1.AIMServiceSpec{
		Replicas: ptr.To(int32(3)),
	})

	if got := isvc.Spec.Predictor.MinReplicas; got == nil || *got != 3 {
		t.Errorf("MinReplicas: want *3, got %v", got)
	}
	if got := isvc.Spec.Predictor.MaxReplicas; got != 3 {
		t.Errorf("MaxReplicas: want 3 (HPA disabled, min==max), got %d", got)
	}
	if got := isvc.Annotations[constants.AnnotationKServeAutoscalerClass]; got != constants.AutoscalerClassNone {
		t.Errorf("autoscaler class: want %q (fixed replicas disables HPA), got %q",
			constants.AutoscalerClassNone, got)
	}
}

func TestBuildInferenceServiceFromProfile_Replicas_AutoScalingHandsOffToExternal(t *testing.T) {
	isvc := buildTestISVC(t, aimv1alpha1.AIMServiceSpec{
		MinReplicas: ptr.To(int32(2)),
		MaxReplicas: ptr.To(int32(5)),
	})

	if got := isvc.Spec.Predictor.MinReplicas; got == nil || *got != 2 {
		t.Errorf("MinReplicas: want *2, got %v", got)
	}
	if got := isvc.Spec.Predictor.MaxReplicas; got != 5 {
		t.Errorf("MaxReplicas: want 5, got %d", got)
	}
	// autoscalerClass=external is what tells KServe's KEDA reconciler to
	// short-circuit; the AIMService controller writes the actual
	// ScaledObject (see scaledobject.go). Asserting "keda" here would
	// re-introduce two ScaledObjects fighting over the same Deployment.
	if got := isvc.Annotations[constants.AnnotationKServeAutoscalerClass]; got != constants.AutoscalerClassExternal {
		t.Errorf("autoscaler class: want %q (AIM Engine owns the ScaledObject under min/max), got %q",
			constants.AutoscalerClassExternal, got)
	}
}

// TestBuildInferenceServiceFromProfile_SharedCacheNotOverlaid pins that a
// profile-owned (Shared) cache is NOT overlaid onto the ISVC — the runtime
// mounts it instead, so overlaying it would duplicate the runtime's volume.
func TestBuildInferenceServiceFromProfile_SharedCacheNotOverlaid(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			// Shared is the default; state it explicitly for the reader.
			Caching: &aimv1alpha1.AIMServiceCachingConfig{Mode: aimv1alpha1.CachingModeShared},
		},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:      service,
			profileCache: controllerutils.FetchResult[*aimv1alpha2.AIMProfileCache]{Value: readyCacheFixture()},
		},
		resolvedProfileSpec:   profileSpecWithModelSources(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
		profileCacheReady:     true,
	}
	(&ProfileServiceReconciler{}).composeDerivedNames(context.Background(), &obs)

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("expected non-nil ISVC")
	}
	if len(isvc.Spec.Predictor.Volumes) != 0 {
		t.Errorf("Shared (profile-owned) cache must not be overlaid; got volumes %+v", isvc.Spec.Predictor.Volumes)
	}
	if len(isvc.Spec.Predictor.Model.VolumeMounts) != 0 {
		t.Errorf("Shared (profile-owned) cache must not add model mounts; got %+v", isvc.Spec.Predictor.Model.VolumeMounts)
	}
}

// TestBuildInferenceServiceFromProfile_DedicatedCacheOverlaid pins that a
// service-owned (Dedicated) cache lands on the overlay: the PVC volume, the
// model volume mount, and the framework redirect env (overriding the runtime
// container by name).
func TestBuildInferenceServiceFromProfile_DedicatedCacheOverlaid(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			Caching: &aimv1alpha1.AIMServiceCachingConfig{Mode: aimv1alpha1.CachingModeDedicated},
		},
	}
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:      service,
			profileCache: controllerutils.FetchResult[*aimv1alpha2.AIMProfileCache]{Value: readyCacheFixture()},
		},
		resolvedProfileSpec:   profileSpecWithModelSources(),
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		profileScope:          aimv1alpha1.AIMResolutionScopeNamespace,
		profileCacheReady:     true,
	}
	(&ProfileServiceReconciler{}).composeDerivedNames(context.Background(), &obs)

	isvc := buildInferenceServiceFromProfile(service, obs)
	if isvc == nil {
		t.Fatalf("expected non-nil ISVC")
	}

	if !volumeRefsPVC(isvc.Spec.Predictor.Volumes, "weights-pvc") {
		t.Errorf("Dedicated (service-owned) cache PVC must be overlaid; got volumes %+v", isvc.Spec.Predictor.Volumes)
	}
	model := isvc.Spec.Predictor.Model
	if len(model.VolumeMounts) == 0 {
		t.Errorf("Dedicated cache must add a model volume mount")
	}
	// Redirect env must be present so the runtime loads from the local cache,
	// overriding the runtime container env by name.
	env := envMap(model.Env)
	if env[constants.EnvAIMModelID] == "" {
		t.Errorf("Dedicated cache overlay must carry the %s redirect env", constants.EnvAIMModelID)
	}
}

func TestBuildInferenceServiceFromProfile_DedicatedCacheOverridesDirectVLLMModelReference(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Caching: &aimv1alpha1.AIMServiceCachingConfig{Mode: aimv1alpha1.CachingModeDedicated},
		},
	}
	profileSpec := profileSpecWithModelSources()
	profileSpec.Engine = "vllm"
	profileSpec.AcceleratorVendor = aimv1alpha1.AcceleratorVendorNVIDIA
	cache := readyCacheFixture()
	obs := ServiceObservation{
		ServiceFetchResult: ServiceFetchResult{
			service:      service,
			profileCache: controllerutils.FetchResult[*aimv1alpha2.AIMProfileCache]{Value: cache},
		},
		resolvedProfileSpec:   profileSpec,
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
		profileName:           testProfileA,
		isvcName:              "service-isvc",
		profileCacheReady:     true,
	}

	isvc := buildInferenceServiceFromProfile(service, obs)
	env := envMap(isvc.Spec.Predictor.Model.Env)
	if got := env[constants.EnvAIMVLLMModel]; got != "/workspace/cache/org/model" {
		t.Fatalf("%s = %q, want dedicated cache mount", constants.EnvAIMVLLMModel, got)
	}
	if env["HF_HUB_OFFLINE"] != "1" || env["TRANSFORMERS_OFFLINE"] != "1" {
		t.Fatalf("dedicated direct-vLLM cache must force offline mode: %#v", env)
	}
}

func profileSpecWithModelSources() *aimv1alpha2.AIMProfileSpecCommon {
	spec := sampleProfileSpec()
	spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}
	return spec
}

func readyCacheFixture() *aimv1alpha2.AIMProfileCache {
	return &aimv1alpha2.AIMProfileCache{
		Status: aimv1alpha2.AIMProfileCacheStatus{
			Status: constants.AIMStatusReady,
			Artifacts: map[string]aimv1alpha1.AIMResolvedArtifact{
				"weights": {
					Name:                  "weights",
					Model:                 "org/model",
					Status:                constants.AIMStatusReady,
					PersistentVolumeClaim: "weights-pvc",
					MountPoint:            "/workspace/cache/org/model",
				},
			},
		},
	}
}

func volumeRefsPVC(volumes []corev1.Volume, claimName string) bool {
	for _, v := range volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

func envMap(vars []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, e := range vars {
		m[e.Name] = e.Value
	}
	return m
}

func TestPlanProfileCache_CreatesSharedCache(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
		},
	}
	profileSpec := sampleProfileSpec()
	profileSpec.ModelSources = []aimv1alpha1.AIMModelSource{{
		ModelID:   "org/model",
		SourceURI: "hf://org/model",
	}}

	obs := ServiceObservation{
		ServiceFetchResult:  ServiceFetchResult{service: service},
		profileName:         testProfileA,
		profileScope:        aimv1alpha1.AIMResolutionScopeNamespace,
		resolvedProfileSpec: profileSpec,
	}

	cache := planProfileCache(service, obs)
	if cache == nil {
		t.Fatalf("expected profile cache to be planned")
	}
	if cache.Namespace != "ns" {
		t.Errorf("unexpected namespace: %q", cache.Namespace)
	}
	if cache.Spec.ProfileName != testProfileA {
		t.Errorf("unexpected profile name on cache spec: %q", cache.Spec.ProfileName)
	}
	if cache.Spec.Mode != aimv1alpha2.ProfileCacheModeShared {
		t.Errorf("expected shared cache mode, got %q", cache.Spec.Mode)
	}
	if cache.Labels[constants.LabelService] != testServiceName {
		t.Errorf("expected service label on cache, got %v", cache.Labels)
	}
}

// TestPlanProfileCache_ServiceCachingEnvReachesCache locks the regression fix:
// for a cluster-scoped (or overlay) profile there is no profile caching.env, so
// the service's spec.caching.env is the only credential source that reaches the
// download Job. obs.profile.Value is nil here to model the cluster-scoped path.
func TestPlanProfileCache_ServiceCachingEnvReachesCache(t *testing.T) {
	token := corev1.EnvVar{
		Name: "HF_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "hf-token"},
				Key:                  "token",
			},
		},
	}
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			Caching: &aimv1alpha1.AIMServiceCachingConfig{Env: []corev1.EnvVar{token}},
		},
	}
	profileSpec := sampleProfileSpec()
	profileSpec.ModelSources = []aimv1alpha1.AIMModelSource{{
		ModelID:   "org/model",
		SourceURI: "hf://org/model",
	}}

	obs := ServiceObservation{
		ServiceFetchResult:  ServiceFetchResult{service: service},
		profileName:         testProfileA,
		profileScope:        aimv1alpha1.AIMResolutionScopeCluster,
		resolvedProfileSpec: profileSpec,
	}

	cache := planProfileCache(service, obs)
	if cache == nil {
		t.Fatalf("expected profile cache to be planned")
	}
	if len(cache.Spec.Env) != 1 || cache.Spec.Env[0].Name != "HF_TOKEN" {
		t.Fatalf("service caching.env must reach cache.spec.env, got %+v", cache.Spec.Env)
	}
	if cache.Spec.Env[0].ValueFrom == nil || cache.Spec.Env[0].ValueFrom.SecretKeyRef == nil {
		t.Errorf("secretKeyRef must be preserved on the cache env, got %+v", cache.Spec.Env[0])
	}
}

// TestPlanProfileCache_ServiceCachingEnvOverridesProfile verifies the merge
// precedence: when both the namespace profile and the service set caching.env,
// the service value wins on conflicting names.
func TestPlanProfileCache_ServiceCachingEnvOverridesProfile(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			Caching: &aimv1alpha1.AIMServiceCachingConfig{
				Env: []corev1.EnvVar{{Name: "HF_TOKEN", Value: "from-service"}},
			},
		},
	}
	profileSpec := sampleProfileSpec()
	profileSpec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}

	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: testProfileA, Namespace: "ns"},
		Spec: aimv1alpha2.AIMProfileSpec{
			Caching: &aimv1alpha2.AIMProfileCachingConfig{
				Env: []corev1.EnvVar{
					{Name: "HF_TOKEN", Value: "from-profile"},
					{Name: "PROFILE_ONLY", Value: "profile-only-val"},
				},
			},
		},
	}

	obs := ServiceObservation{
		ServiceFetchResult:  ServiceFetchResult{service: service},
		profileName:         testProfileA,
		profileScope:        aimv1alpha1.AIMResolutionScopeNamespace,
		resolvedProfileSpec: profileSpec,
	}
	obs.profile.Value = profile

	cache := planProfileCache(service, obs)
	if cache == nil {
		t.Fatalf("expected profile cache to be planned")
	}
	got := envMap(cache.Spec.Env)
	if got["HF_TOKEN"] != "from-service" {
		t.Errorf("service caching.env must override profile on conflict, got %q", got["HF_TOKEN"])
	}
	if got["PROFILE_ONLY"] != "profile-only-val" {
		t.Errorf("profile-only caching.env key must be preserved, got %q", got["PROFILE_ONLY"])
	}
}

// TestPlanProfileCache_PropagatesRuntimeConfigRef verifies the service's runtime
// config reference flows onto the AIMProfileCache so a named AIMRuntimeConfig's
// env can feed the download Job (instead of always the default-named config).
func TestPlanProfileCache_PropagatesRuntimeConfigRef(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile:          &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			RuntimeConfigRef: aimv1alpha1.RuntimeConfigRef{Name: "fast"},
		},
	}
	profileSpec := sampleProfileSpec()
	profileSpec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}

	obs := ServiceObservation{
		ServiceFetchResult:  ServiceFetchResult{service: service},
		profileName:         testProfileA,
		profileScope:        aimv1alpha1.AIMResolutionScopeNamespace,
		resolvedProfileSpec: profileSpec,
	}

	cache := planProfileCache(service, obs)
	if cache == nil {
		t.Fatalf("expected profile cache to be planned")
	}
	if cache.Spec.Name != "fast" {
		t.Errorf("expected runtimeConfigRef to propagate, got %q", cache.Spec.Name)
	}
}

// TestPlanResources_ScaleToZeroEmitsScaledObject verifies the v1alpha2
// pipeline wires the ScaledObject planner. Asserts at the GVK level so the
// v1alpha1 planner tests remain the source of truth for resource shape.
func TestPlanResources_ScaleToZeroEmitsScaledObject(t *testing.T) {
	t.Setenv(constants.EnvAIMGatewayActivationScope, constants.GatewayActivationScopeHTTPRoute)

	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile:     &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			MinReplicas: ptr.To(int32(0)),
			MaxReplicas: ptr.To(int32(3)),
			AutoScaling: &aimv1alpha1.AIMServiceAutoScaling{
				Metrics: []aimv1alpha1.AIMServiceMetricsSpec{{
					Type: "PodMetric",
					PodMetric: &aimv1alpha1.AIMServicePodMetricSource{
						Metric: &aimv1alpha1.AIMServicePodMetric{
							Backend:     "opentelemetry",
							MetricNames: []string{"vllm:num_requests_running"},
							Query:       "vllm:num_requests_running",
						},
						Target: &aimv1alpha1.AIMServiceMetricTarget{Type: "Value", Value: "1"},
					},
				}},
			},
		},
	}
	service.Spec.Routing = &aimv1alpha1.AIMRuntimeRoutingConfig{Enabled: ptr.To(true)}

	r := &ProfileServiceReconciler{}
	obs := ServiceObservation{
		ServiceFetchResult:  ServiceFetchResult{service: service},
		resolvedProfileSpec: sampleProfileSpec(),
		// Deployable=true mirrors what the AIMProfile reconciler stamps
		// once the profile validates and is what isDeployable() short-
		// circuits on. Setting it here keeps the fixture spec minimal
		// (no ModelSources) so PlanResources does not divert into the
		// AIMProfileCache branch, which would early-return on the
		// unready cache and skip the scale-to-zero resources we are
		// asserting on.
		resolvedProfileStatus: &aimv1alpha2.AIMProfileStatus{
			Status:     constants.AIMStatusReady,
			Deployable: true,
		},
		profileName:      testProfileA,
		profileScope:     aimv1alpha1.AIMResolutionScopeNamespace,
		profileAssembled: true,
	}
	r.composeDerivedNames(context.Background(), &obs)
	if obs.configErr != nil {
		t.Fatalf("composeDerivedNames: %v", obs.configErr)
	}

	pr := r.PlanResources(context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha1.AIMService]{Object: service},
		obs,
	)

	var sawScaledObject bool
	for _, obj := range pr.GetToApply() {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Group == "keda.sh" && gvk.Kind == "ScaledObject" {
			sawScaledObject = true
		}
	}
	if !sawScaledObject {
		t.Errorf("scale-to-zero v1alpha2 service must produce a KEDA ScaledObject")
	}
}

func TestPlanResourcesSchedulesActivationMetricGraceDeadline(t *testing.T) {
	t.Setenv(constants.EnvAIMGatewayActivationScope, constants.GatewayActivationScopeHTTPRoute)
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			MinReplicas: ptr.To(int32(0)),
		},
	}
	service.Spec.Routing = &aimv1alpha1.AIMRuntimeRoutingConfig{Enabled: ptr.To(true)}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ExternalMetricSourceType,
				External: &autoscalingv2.ExternalMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: "s0-test-service"},
					Target: autoscalingv2.MetricTarget{
						Type:  autoscalingv2.ValueMetricType,
						Value: ptr.To(resource.MustParse("1")),
					},
				},
			}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			ObservedGeneration: ptr.To(int64(1)),
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{
				Type:   autoscalingv2.ScalingActive,
				Status: corev1.ConditionFalse,
				Reason: "FailedGetExternalMetric",
			}},
		},
	}
	obs := ServiceObservation{ServiceFetchResult: ServiceFetchResult{
		service: service,
		hpa:     controllerutils.FetchResult[*autoscalingv2.HorizontalPodAutoscaler]{Value: hpa},
		inferenceServicePods: &controllerutils.FetchResult[*corev1.PodList]{
			Value: &corev1.PodList{Items: []corev1.Pod{{}}},
		},
	}}

	plan := (&ProfileServiceReconciler{}).PlanResources(
		context.Background(),
		controllerutils.ReconcileContext[*aimv1alpha1.AIMService]{Object: service},
		obs,
	)
	const gracePeriod = 3 * time.Minute
	if plan.RequeueAfter < gracePeriod-10*time.Second || plan.RequeueAfter > gracePeriod {
		t.Errorf("RequeueAfter=%s, want approximately %s", plan.RequeueAfter, gracePeriod)
	}
}

func TestResolveEffectiveResourcesFromProfile(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
	}
	profileSpec := sampleProfileSpec()
	memBlock := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("48Gi")},
	}

	tests := []struct {
		name   string
		status *aimv1alpha2.AIMProfileStatus
		want   *corev1.ResourceRequirements
	}{
		{
			name:   "nil status returns nil",
			status: nil,
			want:   nil,
		},
		{
			name:   "profile not ready returns nil",
			status: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusProgressing, Resources: memBlock},
			want:   nil,
		},
		{
			name:   "profile ready returns merged resources",
			status: &aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady, Resources: memBlock},
			want:   memBlock,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEffectiveResourcesFromProfile(service, profileSpec, tc.status)
			switch {
			case tc.want == nil:
				if got != nil {
					t.Errorf("want nil, got %+v", got)
				}
			case got == nil:
				t.Errorf("want non-nil, got nil")
			default:
				if got.Limits.Memory().Cmp(*tc.want.Limits.Memory()) != 0 {
					t.Errorf("limits.memory: want %v, got %v", tc.want.Limits.Memory(), got.Limits.Memory())
				}
				if got.Requests.Memory().Cmp(*tc.want.Requests.Memory()) != 0 {
					t.Errorf("requests.memory: want %v, got %v", tc.want.Requests.Memory(), got.Requests.Memory())
				}
			}
		})
	}
}

// TestResolveEffectiveResourcesFromProfile_ServiceOverrideWins verifies the
// service-level Resources override takes precedence over the profile's
// computed resources (matching resolveResourcesFromProfile's precedence).
func TestResolveEffectiveResourcesFromProfile_ServiceOverrideWins(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns"},
		Spec: aimv1alpha1.AIMServiceSpec{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Gi")},
			},
		},
	}
	profileSpec := sampleProfileSpec()
	profileStatus := &aimv1alpha2.AIMProfileStatus{
		Status: constants.AIMStatusReady,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Gi")},
		},
	}

	got := resolveEffectiveResourcesFromProfile(service, profileSpec, profileStatus)
	if got == nil {
		t.Fatal("expected non-nil resources")
	}
	if got.Requests.Memory().Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("service override should win; got %v, want 16Gi", got.Requests.Memory())
	}
}

func TestPlanProfileCache_DedicatedHonorsServiceCachingMode(t *testing.T) {
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testServiceName,
			Namespace: "ns",
			UID:       "service-uid-123",
		},
		Spec: aimv1alpha1.AIMServiceSpec{
			Profile: &aimv1alpha1.AIMServiceProfileConfig{Name: testProfileA},
			Caching: &aimv1alpha1.AIMServiceCachingConfig{
				Mode: aimv1alpha1.CachingModeDedicated,
			},
		},
	}
	profileSpec := sampleProfileSpec()
	profileSpec.ModelSources = []aimv1alpha1.AIMModelSource{{
		ModelID:   "org/model",
		SourceURI: "hf://org/model",
	}}

	obs := ServiceObservation{
		ServiceFetchResult:  ServiceFetchResult{service: service},
		profileName:         testProfileA,
		profileScope:        aimv1alpha1.AIMResolutionScopeNamespace,
		resolvedProfileSpec: profileSpec,
	}

	cache := planProfileCache(service, obs)
	if cache == nil {
		t.Fatalf("expected profile cache to be planned for Dedicated service")
	}
	if cache.Spec.Mode != aimv1alpha2.ProfileCacheModeDedicated {
		t.Fatalf("Dedicated AIMService caching must produce a Dedicated AIMProfileCache, got %q", cache.Spec.Mode)
	}

	// And the planned cache name must match what the dedicated lookup
	// pathway expects, so a follow-up reconcile finds the exact same
	// cache without listing.
	expected, err := GenerateProfileCacheName(testProfileA, "ns", testServiceName, "service-uid-123", aimv1alpha1.CachingModeDedicated, aimv1alpha1.AIMResolutionScopeNamespace)
	if err != nil {
		t.Fatalf("generate expected name: %v", err)
	}
	if cache.Name != expected {
		t.Fatalf("Dedicated cache name does not match deterministic-lookup formula: got %q, want %q", cache.Name, expected)
	}
}

// TestPlanResources_LegacyProfileConfigMapCleanup is the upgrade regression: a
// service upgraded from the old inline path left an orphaned
// <service>-profile-<hash> ConfigMap the new reconcile no longer plans.
// PlanResources must GC exactly that orphan — guarded by owner-ref UID AND the
// managed-by label AND the legacy name shape — and never touch a
// coincidentally-named ConfigMap missing a guard, since a false-positive delete
// of a user object is the worst-case regression.
func TestPlanResources_LegacyProfileConfigMapCleanup(t *testing.T) {
	const serviceUID = "svc-uid-legacy-1"
	service := &aimv1alpha1.AIMService{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: "ns", UID: serviceUID},
	}

	legacyName, err := legacyProfileConfigMapName(service.Name)
	if err != nil {
		t.Fatalf("legacyProfileConfigMapName: %v", err)
	}

	ownedByService := []metav1.OwnerReference{{
		APIVersion: "aim.eai.amd.com/v1alpha1",
		Kind:       "AIMService",
		Name:       service.Name,
		UID:        serviceUID,
		Controller: ptr.To(true),
	}}
	ownedByOther := []metav1.OwnerReference{{
		APIVersion: "aim.eai.amd.com/v1alpha1",
		Kind:       "AIMService",
		Name:       "some-other-service",
		UID:        "a-different-uid",
		Controller: ptr.To(true),
	}}
	managedByAIM := map[string]string{constants.LabelK8sManagedBy: constants.LabelValueManagedBy}

	makeConfigMap := func(owners []metav1.OwnerReference, labels map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            legacyName,
				Namespace:       service.Namespace,
				OwnerReferences: owners,
				Labels:          labels,
			},
		}
	}

	tests := []struct {
		name       string
		configMap  *corev1.ConfigMap
		wantDelete bool
	}{
		{
			name:       "owned by this service + managed-by label + legacy name is deleted",
			configMap:  makeConfigMap(ownedByService, managedByAIM),
			wantDelete: true,
		},
		{
			name:       "legacy name but no owner reference is untouched",
			configMap:  makeConfigMap(nil, managedByAIM),
			wantDelete: false,
		},
		{
			name:       "legacy name and owner reference but missing managed-by label is untouched",
			configMap:  makeConfigMap(ownedByService, nil),
			wantDelete: false,
		},
		{
			name:       "legacy name owned by a different service is untouched",
			configMap:  makeConfigMap(ownedByOther, managedByAIM),
			wantDelete: false,
		},
		{
			name:       "no legacy ConfigMap observed (post-rewrite service) deletes nothing",
			configMap:  nil,
			wantDelete: false,
		},
	}

	r := &ProfileServiceReconciler{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := ServiceObservation{
				ServiceFetchResult: ServiceFetchResult{
					service:                service,
					legacyProfileConfigMap: controllerutils.FetchResult[*corev1.ConfigMap]{Value: tc.configMap},
				},
			}

			plan := r.PlanResources(context.Background(),
				controllerutils.ReconcileContext[*aimv1alpha1.AIMService]{Object: service}, obs)

			var deletedConfigMaps int
			for _, obj := range plan.GetToDelete() {
				configMap, ok := obj.(*corev1.ConfigMap)
				if !ok {
					continue
				}
				if configMap.Name != legacyName || configMap.Namespace != service.Namespace {
					t.Fatalf("plan queued deletion of an unexpected ConfigMap %s/%s", configMap.Namespace, configMap.Name)
				}
				deletedConfigMaps++
			}

			if tc.wantDelete && deletedConfigMaps != 1 {
				t.Fatalf("expected exactly one legacy ConfigMap delete, got %d", deletedConfigMaps)
			}
			if !tc.wantDelete && deletedConfigMaps != 0 {
				t.Fatalf("expected no ConfigMap delete, got %d", deletedConfigMaps)
			}
		})
	}
}
