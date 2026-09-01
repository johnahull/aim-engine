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

package controller

import (
	"context"
	"errors"
	"sort"
	"testing"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	servingv1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/profileyaml"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/runtimeprojection"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

const (
	// testNamespace is the shared namespace used across the mapping-handler tests.
	testNamespace     = "team-a"
	currentProfileUID = "profile-current"
)

// runtimeProjectionScheme registers every type the lazy controller's mapping
// handlers read (ISVCs, shadow SR/ConfigMap, backing profile, cache).
func runtimeProjectionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		aimv1alpha1.AddToScheme,
		aimv1alpha2.AddToScheme,
		kservev1alpha1.AddToScheme,
		servingv1beta1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("AddToScheme() error = %v", err)
		}
	}
	return scheme
}

// makeConsumerISVC builds an InferenceService referencing a KServe
// ServingRuntime or ClusterServingRuntime named runtimeName. An empty
// runtimeName leaves predictor.model.runtime unset (a non-consumer).
func makeConsumerISVC(name, namespace, runtimeName string) *servingv1beta1.InferenceService {
	isvc := &servingv1beta1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if runtimeName != "" {
		isvc.Spec.Predictor.Model = &servingv1beta1.ModelSpec{
			ModelFormat: servingv1beta1.ModelFormat{Name: serving.RuntimeModelFormat},
			Runtime:     ptr.To(runtimeName),
		}
	}
	return isvc
}

func makeManagedServingRuntime(name, namespace string) *kservev1alpha1.ServingRuntime {
	return &kservev1alpha1.ServingRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{constants.LabelK8sManagedBy: constants.LabelValueManagedBy},
		},
	}
}

func makeManagedConfigMap(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{constants.LabelK8sManagedBy: constants.LabelValueManagedBy},
		},
	}
}

func recordLazyFieldOwner(obj client.Object) {
	apiVersion := corev1.SchemeGroupVersion.String()
	if _, ok := obj.(*kservev1alpha1.ServingRuntime); ok {
		apiVersion = kservev1alpha1.SchemeGroupVersion.String()
	}
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager:    runtimeProjectionFieldOwner,
		Operation:  metav1.ManagedFieldsOperationApply,
		APIVersion: apiVersion,
		FieldsType: "FieldsV1",
		FieldsV1:   &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{"f:app.kubernetes.io/managed-by":{}}}}`)},
	}})
}

func makeClusterProfileWithAimID(name, aimID string) *aimv1alpha2.AIMClusterProfile {
	return &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       aimv1alpha2.AIMClusterProfileSpec{AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{AimId: aimID}},
	}
}

func makeProfileCache(name, namespace, profileName string, scope aimv1alpha1.AIMResolutionScope) *aimv1alpha2.AIMProfileCache {
	return &aimv1alpha2.AIMProfileCache{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: aimv1alpha2.AIMProfileCacheSpec{
			ProfileName:  profileName,
			ProfileScope: scope,
		},
	}
}

type patchRecordingClient struct {
	client.Client
	patchCalls int
}

func (c *patchRecordingClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	c.patchCalls++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// newRuntimeProjectionClient builds a fake client that mirrors the production
// InferenceService ServingRuntime/ClusterServingRuntime-name index the mapping
// handlers rely on.
func newRuntimeProjectionClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(runtimeProjectionScheme(t)).
		WithObjects(objs...).
		WithIndex(&servingv1beta1.InferenceService{}, inferenceServiceRuntimeIndexKey, indexInferenceServiceRuntime).
		WithReturnManagedFields().
		Build()
}

// requestKeys renders reconcile requests as sorted "namespace/name" strings for
// order-independent comparison.
func requestKeys(reqs []reconcile.Request) []string {
	keys := make([]string, 0, len(reqs))
	for _, r := range reqs {
		keys = append(keys, r.Namespace+"/"+r.Name)
	}
	sort.Strings(keys)
	return keys
}

func assertRequestKeys(t *testing.T, got []reconcile.Request, want []string) {
	t.Helper()
	sort.Strings(want)
	gotKeys := requestKeys(got)
	if len(gotKeys) != len(want) {
		t.Fatalf("request keys = %v, want %v", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Fatalf("request keys = %v, want %v", gotKeys, want)
		}
	}
}

func TestFindInferenceServicesForClusterProfile(t *testing.T) {
	t.Parallel()

	// Two namespaces consume aim-p1; another ISVC consumes aim-p2; a foreign
	// (non-aim) reference must never be enqueued for the profile.
	consumerA := makeConsumerISVC("svc-a", testNamespace, serving.RuntimeName("p1"))
	consumerB := makeConsumerISVC("svc-b", "team-b", serving.RuntimeName("p1"))
	other := makeConsumerISVC("svc-other", testNamespace, serving.RuntimeName("p2"))
	foreign := makeConsumerISVC("svc-foreign", testNamespace, "my-own-runtime")

	c := newRuntimeProjectionClient(t, consumerA, consumerB, other, foreign)
	r := &RuntimeProjectionReconciler{Client: c}

	got := r.findInferenceServicesForClusterProfile(context.Background(), makeClusterProfileWithAimID("p1", ""))
	assertRequestKeys(t, got, []string{"team-a/" + serving.RuntimeName("p1"), "team-b/" + serving.RuntimeName("p1")})
}

func TestFindInferenceServicesForClusterProfile_ModelSlugReference(t *testing.T) {
	t.Parallel()

	// A profile with an aimId also backs the Reduced-mode model-slug
	// ServingRuntime or ClusterServingRuntime name; an InferenceService
	// referencing that slug must be re-enqueued too.
	aimID := "Qwen/Qwen3-0.6B"
	slugConsumer := makeConsumerISVC("svc-slug", testNamespace, serving.ModelSlugRuntimeName(aimID))
	perProfileConsumer := makeConsumerISVC("svc-per-profile", testNamespace, serving.RuntimeName("primary"))

	c := newRuntimeProjectionClient(t, slugConsumer, perProfileConsumer)
	r := &RuntimeProjectionReconciler{Client: c}

	got := r.findInferenceServicesForClusterProfile(context.Background(), makeClusterProfileWithAimID("primary", aimID))
	assertRequestKeys(t, got, []string{
		"team-a/" + serving.RuntimeName("primary"),
		"team-a/" + serving.ModelSlugRuntimeName(aimID),
	})
}

func TestFindInferenceServicesForClusterProfile_WrongType(t *testing.T) {
	t.Parallel()

	c := newRuntimeProjectionClient(t)
	r := &RuntimeProjectionReconciler{Client: c}
	if got := r.findInferenceServicesForClusterProfile(context.Background(), &corev1.ConfigMap{}); got != nil {
		t.Fatalf("expected nil for a non-profile object, got %v", got)
	}
}

// makeNamespaceProfile builds a namespace AIMProfile for the mapping-handler
// fan-out tests. A namespace profile backs a namespace-scoped lazy shadow, so
// its fan-out is confined to its own namespace.
func makeNamespaceProfile(name, namespace, aimID string) *aimv1alpha2.AIMProfile {
	return &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{AimId: aimID}},
	}
}

func TestFindInferenceServicesForProfile(t *testing.T) {
	t.Parallel()

	// A namespace profile backs same-namespace consumers only: an ISVC in
	// another namespace referencing the same name is a DIFFERENT profile's
	// ServingRuntime and must not be enqueued for this one.
	sameNamespace := makeConsumerISVC("svc-a", testNamespace, serving.RuntimeName("p1"))
	otherNamespace := makeConsumerISVC("svc-b", "team-b", serving.RuntimeName("p1"))
	otherProfile := makeConsumerISVC("svc-other", testNamespace, serving.RuntimeName("p2"))

	c := newRuntimeProjectionClient(t, sameNamespace, otherNamespace, otherProfile)
	r := &RuntimeProjectionReconciler{Client: c}

	got := r.findInferenceServicesForProfile(context.Background(), makeNamespaceProfile("p1", testNamespace, ""))
	assertRequestKeys(t, got, []string{"team-a/" + serving.RuntimeName("p1")})
}

func TestFindInferenceServicesForProfile_WrongType(t *testing.T) {
	t.Parallel()

	c := newRuntimeProjectionClient(t)
	r := &RuntimeProjectionReconciler{Client: c}
	if got := r.findInferenceServicesForProfile(context.Background(), &aimv1alpha2.AIMClusterProfile{}); got != nil {
		t.Fatalf("expected nil for a non-namespace-profile object, got %v", got)
	}
}

func TestProfileProjectionPredicate(t *testing.T) {
	t.Parallel()

	p := profileProjectionPredicate()

	base := makeNamespaceProfile("p1", testNamespace, "qwen/qwen3-0.6b")
	base.Generation = 1
	base.Status.Status = constants.AIMStatusPending

	specChanged := base.DeepCopy()
	specChanged.Generation = 2

	readyChanged := base.DeepCopy()
	readyChanged.Status.Status = constants.AIMStatusReady

	contractBackfilled := base.DeepCopy()
	contractBackfilled.Annotations = map[string]string{
		profileyaml.AnnotationContract: profileyaml.DefaultContract().Encode(),
	}

	contractCorrected := contractBackfilled.DeepCopy()
	contractCorrected.Annotations[profileyaml.AnnotationContract] = `{"codec":"aim-profile/v1","metadataFields":["engine","gpu","gpu_count","metric","precision","type"]}`

	cosmetic := base.DeepCopy()

	tests := []struct {
		name string
		old  *aimv1alpha2.AIMProfile
		new  *aimv1alpha2.AIMProfile
		want bool
	}{
		{name: "spec (generation) change fires", old: base, new: specChanged, want: true},
		{name: "readiness transition fires", old: base, new: readyChanged, want: true},
		{name: "contract backfill fires", old: base, new: contractBackfilled, want: true},
		{name: "contract correction fires", old: contractBackfilled, new: contractCorrected, want: true},
		{name: "cosmetic status write is filtered", old: base, new: cosmetic, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Fatalf("Update() = %v, want %v", got, tc.want)
			}
		})
	}

	if !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("Delete should enqueue namespaced profile ServingRuntime keys")
	}
}

// TestFindInferenceServicesForProfileCache is the regression guard for the
// existing per-profile-only fan-out and namespace confinement: with no backing
// profile present, a cache event cannot recover an aimId, so it must enqueue the
// per-profile consumers in the cache's namespace only — unchanged by the
// model-slug widening.
func TestFindInferenceServicesForProfileCache(t *testing.T) {
	t.Parallel()

	inNamespace := makeConsumerISVC("svc-a", testNamespace, serving.RuntimeName("p1"))
	otherNamespace := makeConsumerISVC("svc-b", "team-b", serving.RuntimeName("p1"))
	otherProfile := makeConsumerISVC("svc-other", testNamespace, serving.RuntimeName("p2"))

	c := newRuntimeProjectionClient(t, inNamespace, otherNamespace, otherProfile)
	r := &RuntimeProjectionReconciler{Client: c}

	tests := []struct {
		name  string
		cache *aimv1alpha2.AIMProfileCache
		want  []string
	}{
		{
			name:  "cluster-scope cache fans out to same-namespace consumers only",
			cache: makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeCluster),
			want:  []string{"team-a/" + serving.RuntimeName("p1")},
		},
		{
			name:  "namespace-scope cache fans out to same-namespace consumers (namespace-profile shadow)",
			cache: makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeNamespace),
			want:  []string{"team-a/" + serving.RuntimeName("p1")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.findInferenceServicesForProfileCache(context.Background(), tc.cache)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("expected no requests, got %v", requestKeys(got))
				}
				return
			}
			assertRequestKeys(t, got, tc.want)
		})
	}
}

// TestFindInferenceServicesForProfileCache_ModelSlug covers the fix: a cache
// event recovers its backing profile's aimId — at the cache's declared scope —
// and fans out to BOTH the per-profile and the model-slug primary consumers, so
// a late-Ready cache wakes a Reduced/Both model-slug consumer. It also pins
// namespace confinement (a slug consumer in another namespace is excluded) and,
// via exact-set assertion, that both names are returned without duplicates.
func TestFindInferenceServicesForProfileCache_ModelSlug(t *testing.T) {
	t.Parallel()

	const aimID = "Qwen/Qwen3-0.6B"
	perProfileName := serving.RuntimeName("p1")
	slugName := serving.ModelSlugRuntimeName(aimID)

	tests := []struct {
		name    string
		backing client.Object
		cache   *aimv1alpha2.AIMProfileCache
	}{
		{
			name:    "cluster-scope cache recovers aimId from its cluster profile",
			backing: makeClusterProfileWithAimID("p1", aimID),
			cache:   makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeCluster),
		},
		{
			name:    "namespace-scope cache recovers aimId from its namespace profile",
			backing: makeNamespaceProfile("p1", testNamespace, aimID),
			cache:   makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeNamespace),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A per-profile consumer and a model-slug consumer for the same
			// profile in the cache's namespace, plus noise that must be excluded:
			// a different profile's ServingRuntime, and a slug consumer in another
			// namespace (fan-out is confined to the cache's namespace).
			perProfile := makeConsumerISVC("svc-per-profile", testNamespace, perProfileName)
			slug := makeConsumerISVC("svc-slug", testNamespace, slugName)
			otherProfile := makeConsumerISVC("svc-other", testNamespace, serving.RuntimeName("p2"))
			slugElsewhere := makeConsumerISVC("svc-slug-elsewhere", "team-b", slugName)

			c := newRuntimeProjectionClient(t, tc.backing, perProfile, slug, otherProfile, slugElsewhere)
			r := &RuntimeProjectionReconciler{Client: c}

			got := r.findInferenceServicesForProfileCache(context.Background(), tc.cache)
			assertRequestKeys(t, got, []string{"team-a/" + perProfileName, "team-a/" + slugName})
		})
	}
}

// TestFindInferenceServicesForProfileCache_MissingBackingProfile pins the
// best-effort NotFound fallback: when the cache's backing profile does not
// exist, the aimId cannot be recovered, so the model-slug consumer is dropped
// and only the per-profile consumer is enqueued — no panic, no lost event.
func TestFindInferenceServicesForProfileCache_MissingBackingProfile(t *testing.T) {
	t.Parallel()

	const aimID = "Qwen/Qwen3-0.6B"
	perProfile := makeConsumerISVC("svc-per-profile", testNamespace, serving.RuntimeName("p1"))
	slug := makeConsumerISVC("svc-slug", testNamespace, serving.ModelSlugRuntimeName(aimID))

	c := newRuntimeProjectionClient(t, perProfile, slug)
	r := &RuntimeProjectionReconciler{Client: c}

	cache := makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeCluster)
	got := r.findInferenceServicesForProfileCache(context.Background(), cache)
	assertRequestKeys(t, got, []string{"team-a/" + serving.RuntimeName("p1")})
}

// TestFindInferenceServicesForProfileCache_LookupError pins the non-NotFound
// degradation: a transient/RBAC failure resolving the backing profile is logged
// and degrades to the per-profile fan-out (the model-slug consumer is dropped
// this round, to be re-enqueued on the next event) rather than panicking or
// dropping the cache event entirely.
func TestFindInferenceServicesForProfileCache_LookupError(t *testing.T) {
	t.Parallel()

	const aimID = "Qwen/Qwen3-0.6B"
	perProfile := makeConsumerISVC("svc-per-profile", testNamespace, serving.RuntimeName("p1"))
	slug := makeConsumerISVC("svc-slug", testNamespace, serving.ModelSlugRuntimeName(aimID))
	backing := makeClusterProfileWithAimID("p1", aimID)

	lookupErr := errors.New("boom: forbidden getting AIMClusterProfile")
	c := fake.NewClientBuilder().
		WithScheme(runtimeProjectionScheme(t)).
		WithObjects(backing, perProfile, slug).
		WithIndex(&servingv1beta1.InferenceService{}, inferenceServiceRuntimeIndexKey, indexInferenceServiceRuntime).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*aimv1alpha2.AIMClusterProfile); ok {
					return lookupErr
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &RuntimeProjectionReconciler{Client: c}

	cache := makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeCluster)
	got := r.findInferenceServicesForProfileCache(context.Background(), cache)
	assertRequestKeys(t, got, []string{"team-a/" + serving.RuntimeName("p1")})
}

func TestFindInferenceServicesForManagedRuntimeObject(t *testing.T) {
	t.Parallel()

	consumer := makeConsumerISVC("svc-a", testNamespace, serving.RuntimeName("p1"))
	otherNamespaceConsumer := makeConsumerISVC("svc-b", "team-b", serving.RuntimeName("p1"))

	c := newRuntimeProjectionClient(t, consumer, otherNamespaceConsumer)
	r := &RuntimeProjectionReconciler{Client: c}

	tests := []struct {
		name string
		obj  client.Object
		want []string
	}{
		{
			name: "managed shadow ServingRuntime maps to its same-namespace consumer",
			obj:  makeManagedServingRuntime(serving.RuntimeName("p1"), testNamespace),
			want: []string{"team-a/" + serving.RuntimeName("p1")},
		},
		{
			name: "managed shadow ConfigMap maps to its same-namespace consumer",
			obj:  makeManagedConfigMap(serving.RuntimeName("p1"), testNamespace),
			want: []string{"team-a/" + serving.RuntimeName("p1")},
		},
		{
			name: "unmanaged object (no managed-by label) is ignored",
			obj: &kservev1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: serving.RuntimeName("p1"), Namespace: testNamespace},
			},
			want: nil,
		},
		{
			name: "hand-authored name outside the reserved prefix is ignored",
			obj: &kservev1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-own-runtime",
					Namespace: testNamespace,
					Labels:    map[string]string{constants.LabelK8sManagedBy: constants.LabelValueManagedBy},
				},
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.findInferenceServicesForManagedRuntimeObject(context.Background(), tc.obj)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("expected no requests, got %v", requestKeys(got))
				}
				return
			}
			assertRequestKeys(t, got, tc.want)
		})
	}
}

func TestIsManagedRuntimeObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{
			name: "managed prefix and label",
			obj:  makeManagedServingRuntime(serving.RuntimeName("p1"), testNamespace),
			want: true,
		},
		{
			name: "reserved prefix but missing managed-by label",
			obj: &kservev1alpha1.ServingRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: serving.RuntimeName("p1"), Namespace: testNamespace},
			},
			want: false,
		},
		{
			name: "managed-by label but foreign name",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "my-own-cm",
					Labels: map[string]string{constants.LabelK8sManagedBy: constants.LabelValueManagedBy},
				},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isManagedRuntimeObject(tc.obj); got != tc.want {
				t.Fatalf("isManagedRuntimeObject() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagedRuntimeObjectPredicate(t *testing.T) {
	t.Parallel()

	managed := makeShadowServingRuntime(serving.RuntimeName("p1"), testNamespace, true)
	unmanaged := &kservev1alpha1.ServingRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: serving.RuntimeName("p1"), Namespace: testNamespace},
	}
	p := managedRuntimeObjectPredicate()

	// The controller's own create should not re-trigger a redundant reconcile.
	if p.Create(event.CreateEvent{Object: managed}) {
		t.Error("Create should be filtered for managed objects")
	}
	// Drift (update) and delete of a managed object must re-trigger the apply.
	if !p.Update(event.UpdateEvent{ObjectOld: managed, ObjectNew: managed}) {
		t.Error("Update should fire for a managed object")
	}
	if !p.Delete(event.DeleteEvent{Object: managed}) {
		t.Error("Delete should fire for a managed object")
	}
	markerRemoved := managed.DeepCopy()
	delete(markerRemoved.Labels, constants.LabelRuntimeProjection)
	if !p.Update(event.UpdateEvent{ObjectOld: managed, ObjectNew: markerRemoved}) {
		t.Error("Update should fire when drift removes the lazy marker")
	}
	// Unmanaged objects are ignored entirely.
	if p.Update(event.UpdateEvent{ObjectOld: unmanaged, ObjectNew: unmanaged}) {
		t.Error("Update should be filtered for unmanaged objects")
	}
	if p.Delete(event.DeleteEvent{Object: unmanaged}) {
		t.Error("Delete should be filtered for unmanaged objects")
	}
}

func TestClusterProfileProjectionPredicate(t *testing.T) {
	t.Parallel()

	p := clusterProfileProjectionPredicate()

	base := makeClusterProfileWithAimID("p1", "qwen/qwen3-0.6b")
	base.Generation = 1
	base.Status.Status = constants.AIMStatusPending

	specChanged := base.DeepCopy()
	specChanged.Generation = 2

	readyChanged := base.DeepCopy()
	readyChanged.Status.Status = constants.AIMStatusReady

	contractBackfilled := base.DeepCopy()
	contractBackfilled.Annotations = map[string]string{
		profileyaml.AnnotationContract: profileyaml.DefaultContract().Encode(),
	}

	contractCorrected := contractBackfilled.DeepCopy()
	contractCorrected.Annotations[profileyaml.AnnotationContract] = `{"codec":"aim-profile/v1","metadataFields":["engine","gpu","gpu_count","metric","precision","type"]}`

	cosmetic := base.DeepCopy()

	tests := []struct {
		name string
		old  *aimv1alpha2.AIMClusterProfile
		new  *aimv1alpha2.AIMClusterProfile
		want bool
	}{
		{name: "spec (generation) change fires", old: base, new: specChanged, want: true},
		{name: "readiness transition fires", old: base, new: readyChanged, want: true},
		{name: "contract backfill fires", old: base, new: contractBackfilled, want: true},
		{name: "contract correction fires", old: contractBackfilled, new: contractCorrected, want: true},
		{name: "cosmetic status write is filtered", old: base, new: cosmetic, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Fatalf("Update() = %v, want %v", got, tc.want)
			}
		})
	}

	if !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("Delete should enqueue cluster-profile ServingRuntime keys")
	}
}

func TestInferenceServiceProjectionPredicate(t *testing.T) {
	t.Parallel()

	p := inferenceServiceProjectionPredicate()

	base := makeConsumerISVC("svc", testNamespace, serving.RuntimeName("p1"))
	base.Generation = 1
	base.Annotations = map[string]string{constants.AnnotationRuntimeProfile: "p1"}

	specChanged := base.DeepCopy()
	specChanged.Generation = 2

	annotationChanged := base.DeepCopy()
	annotationChanged.Annotations = map[string]string{constants.AnnotationRuntimeProfile: "p2"}

	// A status-only write bumps neither generation nor the runtime-profile
	// annotation (KServe writes url / conditions / modelStatus here).
	statusOnly := base.DeepCopy()
	statusOnly.Status.ObservedGeneration = 7

	tests := []struct {
		name string
		old  *servingv1beta1.InferenceService
		new  *servingv1beta1.InferenceService
		want bool
	}{
		{name: "spec (generation) change fires", old: base, new: specChanged, want: true},
		{name: "runtime-profile annotation change fires", old: base, new: annotationChanged, want: true},
		{name: "status-only write is filtered", old: base, new: statusOnly, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			if got != tc.want {
				t.Fatalf("Update() = %v, want %v", got, tc.want)
			}
		})
	}

	// A create (new ISVC) must still be projected.
	if !p.Create(event.CreateEvent{Object: base}) {
		t.Error("Create should fire for an InferenceService")
	}
}

// makeShadowServingRuntime builds a complete-looking managed ServingRuntime.
// When lazy is true it carries both identities left by this controller's SSA;
// when false it stands in for a hand-authored ServingRuntime that must be
// deferred to.
func makeShadowServingRuntime(name, namespace string, lazy bool) *kservev1alpha1.ServingRuntime {
	sr := makeManagedServingRuntime(name, namespace)
	if lazy {
		sr.Labels[constants.LabelRuntimeProjection] = constants.LabelValueRuntimeProjectionLazy
		recordLazyFieldOwner(sr)
	}
	return sr
}

// makeEagerServingRuntime builds a managed ServingRuntime carrying the
// eager-projection marker — the shape the profile reconciler force-applies (and
// the shape a lazy shadow takes once the eager path reclaims its marker on a mode
// flip). The lazy path must defer to it.
func makeEagerServingRuntime(name, namespace string) *kservev1alpha1.ServingRuntime {
	sr := makeManagedServingRuntime(name, namespace)
	sr.Labels[constants.LabelRuntimeProjection] = constants.LabelValueRuntimeProjectionEager
	return sr
}

func TestOwnedByLazyProjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		label      string
		fieldOwner string
		want       bool
	}{
		{name: "lazy-projection marker is our shadow", label: constants.LabelValueRuntimeProjectionLazy, want: true},
		{name: "lazy field manager survives a removed marker", fieldOwner: runtimeProjectionFieldOwner, want: true},
		{name: "explicit eager marker wins over stale lazy field ownership", label: constants.LabelValueRuntimeProjectionEager, fieldOwner: runtimeProjectionFieldOwner, want: false},
		{name: "eager-projection marker is not ours (defer)", label: constants.LabelValueRuntimeProjectionEager, want: false},
		{name: "no marker is a hand-authored ServingRuntime (defer)", label: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sr := makeManagedServingRuntime("aim-p1", testNamespace)
			if tc.label != "" {
				sr.Labels[constants.LabelRuntimeProjection] = tc.label
			}
			if tc.fieldOwner != "" {
				sr.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: tc.fieldOwner}}
			}
			if got := ownedByLazyProjection(sr); got != tc.want {
				t.Fatalf("ownedByLazyProjection() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNamespaceRuntimeComplete guards the self-heal fix: this controller's lazy
// shadow must NOT be treated as complete, even after both marker labels are
// removed, while eager and hand-authored pairs remain deferred to.
func TestNamespaceRuntimeComplete(t *testing.T) {
	t.Parallel()

	name := serving.RuntimeName("p1")
	ns := testNamespace
	unmarkedLazySR := makeShadowServingRuntime(name, ns, true)
	delete(unmarkedLazySR.Labels, constants.LabelRuntimeProjection)
	unmarkedLazyCM := makeLazyConfigMap(name, ns)
	delete(unmarkedLazyCM.Labels, constants.LabelRuntimeProjection)

	tests := []struct {
		name string
		objs []client.Object
		want bool
	}{
		{
			name: "our lazy shadow (marker present) is re-applied, not deferred",
			objs: []client.Object{makeShadowServingRuntime(name, ns, true), makeManagedConfigMap(name, ns)},
			want: false,
		},
		{
			name: "eager projection (eager marker) is deferred to",
			objs: []client.Object{makeEagerServingRuntime(name, ns), makeManagedConfigMap(name, ns)},
			want: true,
		},
		{
			name: "our lazy shadow remains managed when both markers are removed",
			objs: []client.Object{unmarkedLazySR, unmarkedLazyCM},
			want: false,
		},
		{
			name: "hand-authored (no marker) is deferred to",
			objs: []client.Object{makeShadowServingRuntime(name, ns, false), makeManagedConfigMap(name, ns)},
			want: true,
		},
		{
			name: "missing ServingRuntime is not complete",
			objs: []client.Object{makeManagedConfigMap(name, ns)},
			want: false,
		},
		{
			name: "ServingRuntime without its colocated ConfigMap is not complete",
			objs: []client.Object{makeShadowServingRuntime(name, ns, false)},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newRuntimeProjectionClient(t, tc.objs...)
			r := &RuntimeProjectionReconciler{Client: c}
			got, err := r.namespaceRuntimeComplete(context.Background(), ns, name)
			if err != nil {
				t.Fatalf("namespaceRuntimeComplete() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("namespaceRuntimeComplete() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNamespaceRuntimeComplete_ConfigMapReadUsesAPIReader proves the colocated
// ConfigMap read goes through APIReader, not the (label-scoped) cached client.
// A hand-authored complete ServingRuntime's ConfigMap carries no managed-by
// label and so is absent from the scoped cache; reading it via the cached client
// would make the ServingRuntime look incomplete and the lazy path would wrongly
// shadow over it. The reader split is simulated with two independent fake
// clients: the ServingRuntime in the cached client, the ConfigMap only in the
// APIReader.
func TestNamespaceRuntimeComplete_ConfigMapReadUsesAPIReader(t *testing.T) {
	t.Parallel()

	name := serving.RuntimeName("p1")
	ns := testNamespace

	// Cached client holds only the (hand-authored, no marker) ServingRuntime.
	cached := newRuntimeProjectionClient(t, makeShadowServingRuntime(name, ns, false))
	// APIReader holds the colocated ConfigMap the scoped cache would not.
	apiReader := newRuntimeProjectionClient(t, makeManagedConfigMap(name, ns))

	r := &RuntimeProjectionReconciler{Client: cached, APIReader: apiReader}
	got, err := r.namespaceRuntimeComplete(context.Background(), ns, name)
	if err != nil {
		t.Fatalf("namespaceRuntimeComplete() error = %v", err)
	}
	if !got {
		t.Fatal("expected complete=true: the ConfigMap must be read via APIReader, not the cached client")
	}

	// Control: with the ConfigMap absent from the APIReader too, the
	// ServingRuntime is correctly seen as incomplete — confirming the ConfigMap
	// read is not served from the cached client (which has no ConfigMap here
	// either).
	rNoCM := &RuntimeProjectionReconciler{Client: cached, APIReader: newRuntimeProjectionClient(t)}
	got, err = rNoCM.namespaceRuntimeComplete(context.Background(), ns, name)
	if err != nil {
		t.Fatalf("namespaceRuntimeComplete() error = %v", err)
	}
	if got {
		t.Fatal("expected complete=false when the ConfigMap is absent from the APIReader")
	}
}

// TestEagerMarkerSettlesOwnershipOnModeFlip is the deterministic proof that a
// mode flip settles ownership. It drives the lazy reconcile's resolve ->
// DesiredFor seam across a flip for a namespace-profile-backed ServingRuntime,
// using the SAME hashed name for both projectors (hashing deliberately does NOT
// separate them):
//
//   - Pre-flip (Reduced): only the lazy shadow exists (runtime-projection=lazy).
//     namespaceRuntimeComplete is false, so the lazy path re-applies its shadow
//     to self-heal — exactly the behaviour that dual-force-applies once eager
//     also turns on.
//   - Post-flip (Reduced -> Exhaustive/Both): the eager force-apply has reclaimed
//     the marker key and overwritten it with runtime-projection=eager.
//     namespaceRuntimeComplete flips to true and DesiredFor materialises nothing,
//     so the lazy path DEFERS and ownership settles on the single eager manager
//     (no more managedFields/resourceVersion ping-pong).
//   - Reverse flip (Exhaustive -> Reduced): the eager per-profile SR is no longer
//     emitted but SURVIVES (additive teardown, eager marker intact). The lazy
//     path still defers to it (not stranded, not re-shadowed); and if it is later
//     removed (ownerRef GC), the lazy path is free to re-shadow it.
func TestEagerMarkerSettlesOwnershipOnModeFlip(t *testing.T) {
	t.Parallel()

	const profileName = "flip-ns-profile"
	runtimeName := serving.RuntimeName(profileName)
	ns := testNamespace

	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: ns},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{
				AimId:           "Qwen/Qwen3-0.6B",
				ModelId:         "Qwen/Qwen3-0.6B",
				Engine:          "vllm",
				Metric:          aimv1alpha1.AIMMetric("latency"),
				Precision:       aimv1alpha1.AIMPrecision("bf16"),
				AcceleratorType: aimv1alpha1.AcceleratorType("cpu"),
				Image:           "registry.example.com/aim-cpu-base:0.12.0",
				ModelSources:    []aimv1alpha1.AIMModelSource{{ModelID: "Qwen/Qwen3-0.6B", SourceURI: "hf://Qwen/Qwen3-0.6B"}},
			},
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady, Deployable: true},
	}
	// The AIMService-stamped annotation is what lets the lazy resolver reach the
	// namespace profile (there is no name-lookup fallback), so DesiredFor would
	// materialise a shadow whenever no eager owner exists.
	isvc := makeConsumerISVC("consumer", ns, runtimeName)
	isvc.Annotations = map[string]string{constants.AnnotationRuntimeProfile: profileName}
	configMap := makeManagedConfigMap(runtimeName, ns)

	resolveDesired := func(t *testing.T, objs ...client.Object) runtimeprojection.DesiredProjection {
		t.Helper()
		c := newRuntimeProjectionClient(t, objs...)
		r := &RuntimeProjectionReconciler{Client: c}
		state, err := r.resolveState(context.Background(), client.ObjectKey{
			Namespace: isvc.Namespace,
			Name:      runtimeprojection.ReferencedRuntimeName(isvc),
		})
		if err != nil {
			t.Fatalf("resolveState() error = %v", err)
		}
		desired, err := runtimeprojection.DesiredFor(isvc, state)
		if err != nil {
			t.Fatalf("DesiredFor() error = %v", err)
		}
		return desired
	}

	t.Run("pre-flip lazy shadow is re-applied (not deferred)", func(t *testing.T) {
		desired := resolveDesired(t,
			profile.DeepCopy(), isvc.DeepCopy(),
			makeShadowServingRuntime(runtimeName, ns, true), configMap.DeepCopy())
		if desired.Runtime == nil {
			t.Fatalf("pre-flip: the lazy path must re-apply its own shadow to self-heal")
		}
	})

	t.Run("post-flip eager marker makes the lazy path defer (ownership settles)", func(t *testing.T) {
		desired := resolveDesired(t,
			profile.DeepCopy(), isvc.DeepCopy(),
			makeEagerServingRuntime(runtimeName, ns), configMap.DeepCopy())
		if desired.Runtime != nil {
			t.Fatalf("post-flip: the lazy path must defer once the eager projection owns the ServingRuntime, got %v", desired.Runtime.Name)
		}
	})

	t.Run("reverse flip: surviving eager ServingRuntime is deferred to, not re-shadowed", func(t *testing.T) {
		desired := resolveDesired(t,
			profile.DeepCopy(), isvc.DeepCopy(),
			makeEagerServingRuntime(runtimeName, ns), configMap.DeepCopy())
		if desired.Runtime != nil {
			t.Fatalf("reverse flip: a surviving eager ServingRuntime must not be re-shadowed, got %v", desired.Runtime.Name)
		}
	})

	t.Run("reverse flip: lazy is free to re-shadow once the eager ServingRuntime is gone", func(t *testing.T) {
		// The eager per-profile ServingRuntime was garbage-collected (e.g.
		// profile deleted and recreated, or the ServingRuntime removed out of
		// band). With no complete owner present, the lazy path re-materialises
		// its own shadow, marked lazy.
		desired := resolveDesired(t, profile.DeepCopy(), isvc.DeepCopy())
		if desired.Runtime == nil {
			t.Fatalf("reverse flip: the lazy path must be free to re-shadow when no ServingRuntime exists")
		}
		if got := desired.Runtime.Labels[constants.LabelRuntimeProjection]; got != constants.LabelValueRuntimeProjectionLazy {
			t.Errorf("re-shadowed ServingRuntime marker = %q, want %q", got, constants.LabelValueRuntimeProjectionLazy)
		}
	})
}

// TestResolveState_NamespaceProfileBacking: a namespaced ISVC referencing the
// hashed per-profile ServingRuntime resolves its namespace AIMProfile via the
// AIMService-stamped annotation, and DesiredFor turns that into a complete
// namespaced ServingRuntime + ConfigMap owned by the profile — keeping a
// Reduced-mode namespace-profile-backed service correct with no eager
// ServingRuntime.
//
// The "no annotation" case pins the consequence of dropping the reverse
// name-lookup resolver: hashed names aren't reversible, so an unannotated ISVC
// resolves to nothing. Cross-scope resolution relies solely on the stamped
// annotation (or a managed-CSR ownerRef for cluster profiles).
func TestResolveState_NamespaceProfileBacking(t *testing.T) {
	t.Parallel()

	const profileName = "reduced-ns-profile"
	runtimeName := serving.RuntimeName(profileName)

	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profileName, Namespace: testNamespace},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{
				AimId:           "Qwen/Qwen3-0.6B",
				ModelId:         "Qwen/Qwen3-0.6B",
				Engine:          "vllm",
				Metric:          aimv1alpha1.AIMMetric("latency"),
				Precision:       aimv1alpha1.AIMPrecision("bf16"),
				AcceleratorType: aimv1alpha1.AcceleratorType("cpu"),
				Image:           "registry.example.com/aim-cpu-base:0.12.0",
				ModelSources:    []aimv1alpha1.AIMModelSource{{ModelID: "Qwen/Qwen3-0.6B", SourceURI: "hf://Qwen/Qwen3-0.6B"}},
			},
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady, Deployable: true},
	}

	tests := []struct {
		name        string
		annotation  string // stamped runtime-profile annotation, "" to omit
		wantResolve bool   // whether the namespace profile should resolve
	}{
		{name: "annotation fast-path resolves the namespace profile", annotation: profileName, wantResolve: true},
		{name: "no annotation no longer resolves (name lookup removed)", annotation: "", wantResolve: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isvc := makeConsumerISVC("consumer", testNamespace, runtimeName)
			if tc.annotation != "" {
				isvc.Annotations = map[string]string{constants.AnnotationRuntimeProfile: tc.annotation}
			}

			c := newRuntimeProjectionClient(t, profile.DeepCopy(), isvc)
			r := &RuntimeProjectionReconciler{Client: c}

			state, err := r.resolveState(context.Background(), client.ObjectKey{
				Namespace: isvc.Namespace,
				Name:      runtimeprojection.ReferencedRuntimeName(isvc),
			})
			if err != nil {
				t.Fatalf("resolveState() error = %v", err)
			}

			backing := state.BackingProfile()
			if !tc.wantResolve {
				if backing != nil {
					t.Fatalf("expected no resolution without the annotation, got %q", backing.GetName())
				}
				desired, err := runtimeprojection.DesiredFor(isvc, state)
				if err != nil {
					t.Fatalf("DesiredFor() error = %v", err)
				}
				if desired.Runtime != nil {
					t.Fatalf("expected nothing materialized, got %v", desired.Runtime)
				}
				return
			}

			if backing == nil {
				t.Fatalf("expected the namespace profile to resolve, got nil")
			}
			if _, ok := backing.(*aimv1alpha2.AIMProfile); !ok {
				t.Fatalf("expected a namespace AIMProfile, got %T", backing)
			}
			if backing.GetName() != profileName {
				t.Fatalf("resolved profile = %q, want %q", backing.GetName(), profileName)
			}

			desired, err := runtimeprojection.DesiredFor(isvc, state)
			if err != nil {
				t.Fatalf("DesiredFor() error = %v", err)
			}
			if desired.Runtime == nil || desired.ConfigMap == nil {
				t.Fatalf("expected a ServingRuntime and ConfigMap, got servingRuntime=%v configMap=%v", desired.Runtime, desired.ConfigMap)
			}
			if desired.Runtime.Name != runtimeName || desired.Runtime.Namespace != testNamespace {
				t.Errorf("ServingRuntime = %s/%s, want %s/%s", desired.Runtime.Namespace, desired.Runtime.Name, testNamespace, runtimeName)
			}
			if _, ok := desired.Owner.(*aimv1alpha2.AIMProfile); !ok {
				t.Fatalf("expected owner to be the namespace AIMProfile, got %T", desired.Owner)
			}
			if desired.Owner.GetName() != profileName {
				t.Errorf("owner name = %q, want %q", desired.Owner.GetName(), profileName)
			}
		})
	}
}

// TestResolveState_ManagedClusterRuntimeBacking pins the native, annotation-free
// resolution path: a KServe InferenceService referencing a managed
// ClusterServingRuntime resolves the backing AIMClusterProfile via that
// ClusterServingRuntime's profile correlator (ownerRef/label), and DesiredFor
// shadows it into a complete namespaced ServingRuntime.
func TestResolveState_ManagedClusterRuntimeBacking(t *testing.T) {
	t.Parallel()

	const profileName = "qwen3-32b-cluster"
	runtimeName := serving.RuntimeName(profileName)

	profile := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: profileName},
		Spec: aimv1alpha2.AIMClusterProfileSpec{
			AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{
				AimId:           "qwen/qwen3-32b",
				ModelId:         "qwen/qwen3-32b-fp8",
				Engine:          "vllm",
				Metric:          aimv1alpha1.AIMMetric("latency"),
				Precision:       aimv1alpha1.AIMPrecision("fp8"),
				AcceleratorType: aimv1alpha1.AcceleratorType("cpu"),
				Image:           "registry.example.com/aim-base:0.12.0",
				ModelSources:    []aimv1alpha1.AIMModelSource{{ModelID: "qwen/qwen3-32b-fp8", SourceURI: "hf://qwen/qwen3-32b-fp8"}},
			},
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady, Deployable: true},
	}

	// The managed ClusterServingRuntime carries the profile correlator label,
	// which is how the native resolver maps the referenced
	// ClusterServingRuntime back to its backing cluster profile (no annotation,
	// no name parsing).
	managedCSR := &kservev1alpha1.ClusterServingRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name: runtimeName,
			Labels: map[string]string{
				constants.LabelK8sManagedBy: constants.LabelValueManagedBy,
				constants.LabelProfile:      profileName,
			},
		},
	}

	isvc := makeConsumerISVC("consumer", testNamespace, runtimeName)
	c := newRuntimeProjectionClient(t, profile, managedCSR, isvc)
	r := &RuntimeProjectionReconciler{Client: c}

	state, err := r.resolveState(context.Background(), client.ObjectKey{
		Namespace: isvc.Namespace,
		Name:      runtimeprojection.ReferencedRuntimeName(isvc),
	})
	if err != nil {
		t.Fatalf("resolveState() error = %v", err)
	}

	backing := state.BackingProfile()
	if backing == nil {
		t.Fatalf("expected the managed CSR to resolve its backing cluster profile, got nil")
	}
	if _, ok := backing.(*aimv1alpha2.AIMClusterProfile); !ok {
		t.Fatalf("expected an AIMClusterProfile, got %T", backing)
	}
	if backing.GetName() != profileName {
		t.Fatalf("resolved profile = %q, want %q", backing.GetName(), profileName)
	}

	desired, err := runtimeprojection.DesiredFor(isvc, state)
	if err != nil {
		t.Fatalf("DesiredFor() error = %v", err)
	}
	if desired.Runtime == nil || desired.ConfigMap == nil {
		t.Fatalf("expected a ServingRuntime and ConfigMap, got servingRuntime=%v configMap=%v", desired.Runtime, desired.ConfigMap)
	}
	if desired.Runtime.Name != runtimeName || desired.Runtime.Namespace != testNamespace {
		t.Errorf("ServingRuntime = %s/%s, want %s/%s", desired.Runtime.Namespace, desired.Runtime.Name, testNamespace, runtimeName)
	}
	if desired.Owner == nil || desired.Owner.GetName() != profileName {
		t.Errorf("owner = %v, want %q", desired.Owner, profileName)
	}
}

func TestProfileCacheProjectionPredicate(t *testing.T) {
	t.Parallel()

	p := profileCacheProjectionPredicate()

	base := makeProfileCache("pc", testNamespace, "p1", aimv1alpha1.AIMResolutionScopeCluster)
	base.Status.Status = constants.AIMStatusProgressing

	becameReady := base.DeepCopy()
	becameReady.Status.Status = constants.AIMStatusReady

	becameDedicated := base.DeepCopy()
	becameDedicated.Generation++
	becameDedicated.Spec.Mode = aimv1alpha2.ProfileCacheModeDedicated

	statusCaughtUp := base.DeepCopy()
	statusCaughtUp.Status.ObservedGeneration++

	cosmetic := base.DeepCopy()

	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: becameReady}) {
		t.Error("Update should fire when the cache becomes Ready")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: becameDedicated}) {
		t.Error("Update should fire when a spec change removes the cache from shared runtime projection")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusCaughtUp}) {
		t.Error("Update should fire when cache status catches up to its spec generation")
	}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: cosmetic}) {
		t.Error("Update should be filtered for a cosmetic cache write")
	}
	if !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("Delete should fire so the cache mount is reverted")
	}
}

func lazyProfileOwner(profile runtimeprojection.BackingProfile) metav1.OwnerReference {
	kind := clusterProfileKind
	if _, ok := profile.(*aimv1alpha2.AIMProfile); ok {
		kind = namespaceProfileKind
	}
	return metav1.OwnerReference{
		APIVersion: aimv1alpha2.GroupVersion.String(),
		Kind:       kind,
		Name:       profile.GetName(),
		UID:        profile.GetUID(),
		Controller: ptr.To(true),
	}
}

func makeLazyConfigMap(name, namespace string) *corev1.ConfigMap {
	cm := makeManagedConfigMap(name, namespace)
	cm.Labels[constants.LabelRuntimeProjection] = constants.LabelValueRuntimeProjectionLazy
	recordLazyFieldOwner(cm)
	return cm
}

func TestRuntimeKeyForInferenceService(t *testing.T) {
	t.Parallel()

	runtimeName := serving.RuntimeName("p1")
	isvc := makeConsumerISVC("isvc-name-must-not-be-the-key", testNamespace, runtimeName)
	assertRequestKeys(t, runtimeKeyForInferenceService(context.Background(), isvc), []string{
		testNamespace + "/" + runtimeName,
	})

	if got := runtimeKeyForInferenceService(context.Background(), makeConsumerISVC("foreign", testNamespace, "hand-authored")); got != nil {
		t.Fatalf("foreign ServingRuntime name mapped to requests: %v", requestKeys(got))
	}
}

func TestProfileFanoutIncludesInactiveOwnedShadowsAndDedupes(t *testing.T) {
	t.Parallel()

	profile := makeClusterProfileWithAimID("p1", "Qwen/Qwen3-0.6B")
	profile.UID = currentProfileUID
	perProfileName := serving.RuntimeName(profile.Name)
	slugName := serving.ModelSlugRuntimeName(profile.Spec.AimId)
	owner := lazyProfileOwner(profile)

	active := makeConsumerISVC("active", testNamespace, perProfileName)
	activeSR := makeShadowServingRuntime(perProfileName, testNamespace, true)
	activeSR.OwnerReferences = []metav1.OwnerReference{owner}
	activeCM := makeLazyConfigMap(perProfileName, testNamespace)
	activeCM.OwnerReferences = []metav1.OwnerReference{owner}

	inactiveSR := makeShadowServingRuntime(slugName, "team-b", true)
	inactiveSR.OwnerReferences = []metav1.OwnerReference{owner}
	inactiveCM := makeLazyConfigMap(slugName, "team-b")
	inactiveCM.OwnerReferences = []metav1.OwnerReference{owner}
	delete(inactiveSR.Labels, constants.LabelRuntimeProjection)
	delete(inactiveCM.Labels, constants.LabelRuntimeProjection)

	staleOwner := owner
	staleOwner.UID = "profile-old"
	stale := makeShadowServingRuntime(serving.RuntimeName("stale"), "team-c", true)
	stale.OwnerReferences = []metav1.OwnerReference{staleOwner}

	r := &RuntimeProjectionReconciler{Client: newRuntimeProjectionClient(
		t, active, activeSR, activeCM, inactiveSR, inactiveCM, stale,
	)}
	got := r.findInferenceServicesForClusterProfile(context.Background(), profile)
	assertRequestKeys(t, got, []string{
		testNamespace + "/" + perProfileName,
		"team-b/" + slugName,
	})
}

func TestCacheFanoutIncludesInactiveModelSlugShadow(t *testing.T) {
	t.Parallel()

	profile := makeClusterProfileWithAimID("p1", "Qwen/Qwen3-0.6B")
	profile.UID = currentProfileUID
	perProfileName := serving.RuntimeName(profile.Name)
	slugName := serving.ModelSlugRuntimeName(profile.Spec.AimId)
	owner := lazyProfileOwner(profile)

	active := makeConsumerISVC("active", testNamespace, perProfileName)
	inactiveSlug := makeShadowServingRuntime(slugName, testNamespace, true)
	inactiveSlug.OwnerReferences = []metav1.OwnerReference{owner}
	delete(inactiveSlug.Labels, constants.LabelRuntimeProjection)
	elsewhere := makeShadowServingRuntime(slugName, "team-b", true)
	elsewhere.OwnerReferences = []metav1.OwnerReference{owner}

	r := &RuntimeProjectionReconciler{Client: newRuntimeProjectionClient(t, profile, active, inactiveSlug, elsewhere)}
	cache := makeProfileCache("cache", testNamespace, profile.Name, aimv1alpha1.AIMResolutionScopeCluster)
	got := r.findInferenceServicesForProfileCache(context.Background(), cache)
	assertRequestKeys(t, got, []string{
		testNamespace + "/" + perProfileName,
		testNamespace + "/" + slugName,
	})
}

func TestResolveState_OwnerFromEitherPartialLazySibling(t *testing.T) {
	t.Parallel()

	profile := makeClusterProfileWithAimID("p1", "Qwen/Qwen3-0.6B")
	profile.UID = currentProfileUID
	runtimeName := serving.RuntimeName(profile.Name)
	owner := lazyProfileOwner(profile)

	sr := makeShadowServingRuntime(runtimeName, testNamespace, true)
	sr.OwnerReferences = []metav1.OwnerReference{owner}
	cm := makeLazyConfigMap(runtimeName, testNamespace)
	cm.OwnerReferences = []metav1.OwnerReference{owner}

	tests := []struct {
		name string
		obj  client.Object
	}{
		{name: "ServingRuntime remains", obj: sr},
		{name: "ConfigMap remains", obj: cm},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &RuntimeProjectionReconciler{Client: newRuntimeProjectionClient(t, profile.DeepCopyObject().(client.Object), tc.obj)}
			key := client.ObjectKey{Namespace: testNamespace, Name: runtimeName}
			state, err := r.resolveState(context.Background(), key)
			if err != nil {
				t.Fatalf("resolveState() error = %v", err)
			}
			if state.ExistingShadowProfile == nil || state.ExistingShadowProfile.GetUID() != profile.UID {
				t.Fatalf("existing owner = %v, want profile UID %q", state.ExistingShadowProfile, profile.UID)
			}
			desired, err := runtimeprojection.DesiredForRuntime(key.Namespace, key.Name, state)
			if err != nil {
				t.Fatalf("DesiredForRuntime() error = %v", err)
			}
			if desired.Runtime == nil || desired.ConfigMap == nil {
				t.Fatalf("partial deletion must recreate both siblings, got servingRuntime=%v configMap=%v", desired.Runtime, desired.ConfigMap)
			}
		})
	}
}

func TestResolveState_RepairsLazyPairAfterMarkersRemoved(t *testing.T) {
	t.Parallel()

	profile := makeClusterProfileWithAimID("p1", "Qwen/Qwen3-0.6B")
	profile.UID = currentProfileUID
	runtimeName := serving.RuntimeName(profile.Name)
	owner := lazyProfileOwner(profile)

	sr := makeShadowServingRuntime(runtimeName, testNamespace, true)
	sr.OwnerReferences = []metav1.OwnerReference{owner}
	delete(sr.Labels, constants.LabelRuntimeProjection)
	cm := makeLazyConfigMap(runtimeName, testNamespace)
	cm.OwnerReferences = []metav1.OwnerReference{owner}
	delete(cm.Labels, constants.LabelRuntimeProjection)

	r := &RuntimeProjectionReconciler{Client: newRuntimeProjectionClient(t, profile, sr, cm)}
	key := client.ObjectKey{Namespace: testNamespace, Name: runtimeName}
	state, err := r.resolveState(context.Background(), key)
	if err != nil {
		t.Fatalf("resolveState() error = %v", err)
	}
	if state.ExistingShadowProfile == nil || state.ExistingShadowProfile.GetUID() != profile.UID {
		t.Fatalf("existing owner = %v, want profile UID %q", state.ExistingShadowProfile, profile.UID)
	}

	desired, err := runtimeprojection.DesiredForRuntime(key.Namespace, key.Name, state)
	if err != nil {
		t.Fatalf("DesiredForRuntime() error = %v", err)
	}
	if desired.Runtime == nil || desired.ConfigMap == nil {
		t.Fatalf("unmarked lazy pair must be repaired, got servingRuntime=%v configMap=%v", desired.Runtime, desired.ConfigMap)
	}
	if got := desired.Runtime.Labels[constants.LabelRuntimeProjection]; got != constants.LabelValueRuntimeProjectionLazy {
		t.Errorf("repaired ServingRuntime marker = %q, want %q", got, constants.LabelValueRuntimeProjectionLazy)
	}
	if got := desired.ConfigMap.Labels[constants.LabelRuntimeProjection]; got != constants.LabelValueRuntimeProjectionLazy {
		t.Errorf("repaired ConfigMap marker = %q, want %q", got, constants.LabelValueRuntimeProjectionLazy)
	}
}

func TestReconcile_CacheReadinessUnknownPreservesLazyProjection(t *testing.T) {
	profile := makeClusterProfileWithAimID("p1", "")
	profile.UID = currentProfileUID
	runtimeName := serving.RuntimeName(profile.Name)
	owner := lazyProfileOwner(profile)

	sr := makeShadowServingRuntime(runtimeName, testNamespace, true)
	sr.OwnerReferences = []metav1.OwnerReference{owner}
	sr.Spec.Volumes = []corev1.Volume{{
		Name: "cached-model",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "last-known-cache"},
		},
	}}
	cm := makeLazyConfigMap(runtimeName, testNamespace)
	cm.OwnerReferences = []metav1.OwnerReference{owner}

	cache := makeProfileCache(
		"cache",
		testNamespace,
		profile.Name,
		aimv1alpha1.AIMResolutionScopeCluster,
	)
	cache.Spec.Mode = aimv1alpha2.ProfileCacheModeShared
	cache.Status.Conditions = []metav1.Condition{{
		Type:   "DependenciesReachable",
		Status: metav1.ConditionFalse,
		Reason: "ListFailed",
	}}

	recordingClient := &patchRecordingClient{
		Client: newRuntimeProjectionClient(t, profile, sr, cm, cache),
	}
	r := &RuntimeProjectionReconciler{
		Client: recordingClient,
		Scheme: runtimeProjectionScheme(t),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: runtimeName},
	})
	if err == nil {
		t.Fatal("Reconcile() error = nil, want cache readiness error")
	}
	if recordingClient.patchCalls != 0 {
		t.Fatalf("Patch() calls = %d, want 0 while cache readiness is unknown", recordingClient.patchCalls)
	}

	var got kservev1alpha1.ServingRuntime
	if err := recordingClient.Get(
		context.Background(),
		client.ObjectKey{Namespace: testNamespace, Name: runtimeName},
		&got,
	); err != nil {
		t.Fatalf("get existing ServingRuntime: %v", err)
	}
	if len(got.Spec.Volumes) != 1 ||
		got.Spec.Volumes[0].PersistentVolumeClaim == nil ||
		got.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "last-known-cache" {
		t.Fatalf("existing cache volume changed while readiness was unknown: %#v", got.Spec.Volumes)
	}
}

func TestResolveState_RejectsRecreatedSameNameOwner(t *testing.T) {
	t.Parallel()

	profile := makeClusterProfileWithAimID("p1", "")
	profile.UID = "profile-new"
	runtimeName := serving.RuntimeName(profile.Name)
	shadow := makeShadowServingRuntime(runtimeName, testNamespace, true)
	shadow.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: aimv1alpha2.GroupVersion.String(),
		Kind:       clusterProfileKind,
		Name:       profile.Name,
		UID:        "profile-old",
	}}

	r := &RuntimeProjectionReconciler{Client: newRuntimeProjectionClient(t, profile, shadow)}
	state, err := r.resolveState(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: runtimeName})
	if err != nil {
		t.Fatalf("resolveState() error = %v", err)
	}
	if state.BackingProfile() != nil {
		t.Fatalf("stale same-name owner resolved to recreated profile: %v", state.BackingProfile())
	}
}

func TestResolveState_FullyGoneInactiveRuntimeDoesNotBootstrapFromCSR(t *testing.T) {
	t.Parallel()

	profile := makeClusterProfileWithAimID("p1", "")
	runtimeName := serving.RuntimeName(profile.Name)
	csr := &kservev1alpha1.ClusterServingRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name: runtimeName,
			Labels: map[string]string{
				constants.LabelK8sManagedBy: constants.LabelValueManagedBy,
				constants.LabelProfile:      profile.Name,
			},
		},
	}
	r := &RuntimeProjectionReconciler{Client: newRuntimeProjectionClient(t, profile, csr)}
	key := client.ObjectKey{Namespace: testNamespace, Name: runtimeName}
	state, err := r.resolveState(context.Background(), key)
	if err != nil {
		t.Fatalf("resolveState() error = %v", err)
	}
	desired, err := runtimeprojection.DesiredForRuntime(key.Namespace, key.Name, state)
	if err != nil {
		t.Fatalf("DesiredForRuntime() error = %v", err)
	}
	if desired.Runtime != nil {
		t.Fatalf("fully gone inactive ServingRuntime was resurrected: %v", desired.Runtime)
	}
}
