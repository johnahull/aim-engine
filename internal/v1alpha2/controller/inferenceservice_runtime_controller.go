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
	"strings"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	servingv1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/profilecache"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/runtimeprojection"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

const (
	inferenceServiceRuntimeControllerName = "runtime-projection"
	// runtimeProjectionFieldOwner is the SSA field manager for the lazily
	// materialized namespace ServingRuntime + ConfigMap. Distinct from the
	// profile/service field owners so authority over these objects is traceable.
	runtimeProjectionFieldOwner = "aim-runtime-projection-controller"

	// inferenceServiceRuntimeIndexKey indexes InferenceServices by the runtime
	// they reference (spec.predictor.model.runtime). The mapping handlers use it
	// to fan a changed profile / cache / shadow object back to the ISVCs that
	// consume the affected runtime, without paging every ISVC on each event.
	inferenceServiceRuntimeIndexKey = ".spec.predictor.model.runtime"
)

// InferenceServiceRuntimeReconciler watches KServe InferenceServices and lazily
// materializes a complete namespace ServingRuntime (+ colocated profile
// ConfigMap) in the ISVC's namespace when it references a managed runtime that
// does not already resolve to a complete runtime there. The materialized
// objects are owned by the backing AIMClusterProfile (garbage collected on
// profile delete) and applied authoritatively (SSA + ForceOwnership) under the
// reserved aim- prefix.
//
// This reconciler is mode-independent and always on: it guarantees an
// AIMService's runtime (and any native KServe ISVC referencing a managed CSR)
// gets its colocated profile ConfigMap regardless of the eager projection mode.
type InferenceServiceRuntimeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// APIReader is the uncached, direct-to-API reader. It is used only where a
	// read may target an unlabeled ConfigMap the label-scoped cache does not
	// hold (a hand-authored runtime's colocated ConfigMap in
	// namespaceRuntimeComplete). Nil in unit tests, which fall back to Client.
	APIReader client.Reader
	Clientset kubernetes.Interface
}

// configMapReader returns the reader used for ConfigMap point reads that must
// see objects outside the label-scoped cache. Falls back to the cached client
// when APIReader is unset (unit tests wiring only Client).
func (r *InferenceServiceRuntimeReconciler) configMapReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferenceservices,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.kserve.io,resources=servingruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.kserve.io,resources=clusterservingruntimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=aim.eai.amd.com,resources=aimclusterprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=aim.eai.amd.com,resources=aimprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=aim.eai.amd.com,resources=aimprofilecaches,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *InferenceServiceRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var isvc servingv1beta1.InferenceService
	if err := r.Get(ctx, req.NamespacedName, &isvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if isvc.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	state, err := r.resolveState(ctx, &isvc)
	if err != nil {
		return ctrl.Result{}, err
	}

	desired, err := runtimeprojection.DesiredFor(&isvc, state)
	if err != nil {
		return ctrl.Result{}, err
	}
	if desired.Runtime == nil {
		return ctrl.Result{}, nil
	}

	objects := []client.Object{desired.Runtime, desired.ConfigMap}
	if err := controllerutils.ApplyDesiredStateWithForce(
		ctx, r.Client, runtimeProjectionFieldOwner, r.Scheme, objects, desired.Owner,
	); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("materialized namespace serving runtime for referenced runtime",
		"runtime", desired.Runtime.Name,
		"namespace", desired.Runtime.Namespace,
		"profile", desired.Owner.GetName(),
		"inferenceService", isvc.Name)
	return ctrl.Result{}, nil
}

// resolveState reads the cluster state the pure DesiredFor seam needs: whether a
// complete namespace runtime already serves the reference, and the backing
// profile candidates in resolution order.
func (r *InferenceServiceRuntimeReconciler) resolveState(
	ctx context.Context,
	isvc *servingv1beta1.InferenceService,
) (runtimeprojection.ProjectionState, error) {
	var state runtimeprojection.ProjectionState

	runtimeName := runtimeprojection.ReferencedRuntimeName(isvc)
	if runtimeName == "" || !strings.HasPrefix(runtimeName, serving.RuntimeNamePrefix) {
		return state, nil
	}

	complete, err := r.namespaceRuntimeComplete(ctx, isvc.Namespace, runtimeName)
	if err != nil {
		return state, err
	}
	if complete {
		state.NamespaceRuntimeComplete = true
		return state, nil
	}

	state.ManagedRuntimeProfile, err = r.profileFromManagedClusterRuntime(ctx, runtimeName)
	if err != nil {
		return state, err
	}
	state.AnnotatedProfile, err = r.profileFromAnnotation(ctx, isvc)
	if err != nil {
		return state, err
	}

	if profile := state.BackingProfile(); profile != nil {
		state.Cache, err = r.readyProfileCache(ctx, isvc.Namespace, profile.GetName(), backingProfileScope(profile))
		if err != nil {
			return state, err
		}
	}

	return state, nil
}

// backingProfileScope classifies a resolved backing profile so the colocated
// cache lookup matches the profile-owned cache's ProfileScope: a namespace
// AIMProfile pairs with a namespace-scope cache, a cluster AIMClusterProfile
// with a cluster-scope cache.
func backingProfileScope(profile runtimeprojection.BackingProfile) aimv1alpha1.AIMResolutionScope {
	if _, ok := profile.(*aimv1alpha2.AIMProfile); ok {
		return aimv1alpha1.AIMResolutionScopeNamespace
	}
	return aimv1alpha1.AIMResolutionScopeCluster
}

// namespaceRuntimeComplete reports whether a complete namespace runtime — a
// ServingRuntime plus its colocated ConfigMap of the same name — that the lazy
// projection must defer to already exists in the namespace, in which case no
// shadow is materialized.
//
// A complete runtime this controller materialized itself (its lazy shadow) does
// NOT count: it must be re-applied on every reconcile so a drift edit, a
// late-Ready cache, or backing-profile changes are reasserted by the
// authoritative force-apply. Only a complete runtime materialized by something
// else — a profile reconciler's eager projection, or a hand-authored runtime —
// is deferred to.
func (r *InferenceServiceRuntimeReconciler) namespaceRuntimeComplete(
	ctx context.Context,
	namespace, name string,
) (bool, error) {
	var sr kservev1alpha1.ServingRuntime
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// Read the colocated ConfigMap through the uncached reader: the ConfigMap
	// cache is label-scoped to managed-by=aim-engine, so a hand-authored
	// complete runtime's unlabeled ConfigMap is invisible to the cached client
	// and we must not mistake it for "incomplete" and shadow over it.
	var cm corev1.ConfigMap
	if err := r.configMapReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// Our own lazy shadow is never "complete enough to stop" — re-materialize it
	// so drift / late cache / profile changes reconcile.
	if ownedByLazyProjection(&sr) {
		return false, nil
	}
	return true, nil
}

// ownedByLazyProjection reports whether the object is a shadow this controller
// materialized, keyed on the exact lazy-projection marker value.
//
// The marker (not the ownerReference kind) is load-bearing: a
// namespace-profile-backed shadow and that profile's eager per-profile
// projection share the same AIMProfile ownerRef, so the kind can't distinguish
// them, and adopting an eager object would hot-loop against the profile
// controller's force-apply. Only the exact lazy value is ours; the eager value
// and unmarked hand-authored runtimes are deferred to — which is how ownership
// settles once a mode flip has the eager force-apply overwrite the lazy marker.
func ownedByLazyProjection(obj client.Object) bool {
	return obj.GetLabels()[constants.LabelRuntimeProjection] == constants.LabelValueRuntimeProjectionLazy
}

// profileFromManagedClusterRuntime resolves the backing AIMClusterProfile of a
// referenced runtime by following a managed ClusterServingRuntime's
// ownerReference (Kind=AIMClusterProfile) or its profile correlator label. This
// is the native, annotation-free flow; it only ever yields a cluster profile (a
// namespace profile projects a namespace ServingRuntime, never a CSR).
func (r *InferenceServiceRuntimeReconciler) profileFromManagedClusterRuntime(
	ctx context.Context,
	runtimeName string,
) (runtimeprojection.BackingProfile, error) {
	var csr kservev1alpha1.ClusterServingRuntime
	if err := r.Get(ctx, client.ObjectKey{Name: runtimeName}, &csr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	profileName := backingProfileNameFromRuntimeMeta(csr.GetOwnerReferences(), csr.GetLabels())
	if profileName == "" {
		return nil, nil
	}
	return r.clusterBackingProfile(ctx, profileName)
}

// profileFromAnnotation resolves the backing profile from the AIMService-stamped
// runtime-profile annotation on the ISVC — the fast-path for a runtime that has
// not been created yet (cross-scope, or Reduced mode). Resolved namespace-first
// so a namespace-scope service settles on its namespace AIMProfile.
func (r *InferenceServiceRuntimeReconciler) profileFromAnnotation(
	ctx context.Context,
	isvc *servingv1beta1.InferenceService,
) (runtimeprojection.BackingProfile, error) {
	profileName := isvc.GetAnnotations()[constants.AnnotationRuntimeProfile]
	return r.lookupBackingProfile(ctx, isvc.Namespace, profileName)
}

// lookupBackingProfile resolves a backing profile by name, preferring a
// namespace AIMProfile in the given namespace over a cluster AIMClusterProfile
// of the same name. This mirrors the AIMService resolver's namespace-over-cluster
// precedence (internal/v1alpha2/aimservice/resolver.go resolveByName), so the
// lazy resolver settles on the same profile scope the service did. Returns a
// genuine nil interface (never a typed-nil pointer) when neither exists, so the
// first-non-nil resolution order in ProjectionState.BackingProfile stays sound.
func (r *InferenceServiceRuntimeReconciler) lookupBackingProfile(
	ctx context.Context,
	namespace, name string,
) (runtimeprojection.BackingProfile, error) {
	if name == "" {
		return nil, nil
	}

	var nsProfile aimv1alpha2.AIMProfile
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &nsProfile)
	if err == nil {
		return &nsProfile, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	return r.clusterBackingProfile(ctx, name)
}

// clusterBackingProfile resolves a cluster AIMClusterProfile by name, returning a
// genuine nil interface (not a typed-nil pointer) when absent.
func (r *InferenceServiceRuntimeReconciler) clusterBackingProfile(
	ctx context.Context,
	name string,
) (runtimeprojection.BackingProfile, error) {
	var profile aimv1alpha2.AIMClusterProfile
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &profile, nil
}

// readyProfileCache returns a Ready, profile-owned (Shared) AIMProfileCache in
// the namespace that caches the given profile at the given scope, or nil when
// none exists. The cache contributes the profile-owned cache mount on the
// materialized runtime. Scope must match the backing profile's scope: a
// cluster-scope cache for an AIMClusterProfile-backed shadow, a namespace-scope
// cache for a namespace-AIMProfile-backed shadow.
//
// It delegates to the shared profilecache resolver so this lazy shadow and the
// eager profile-reconciler projection mount the identical cache — the fix for
// the mode-dependent Shared-cache mount (a service-driven Shared cache resolves
// here whether or not the profile set caching.enabled).
func (r *InferenceServiceRuntimeReconciler) readyProfileCache(
	ctx context.Context,
	namespace, profileName string,
	scope aimv1alpha1.AIMResolutionScope,
) (*aimv1alpha2.AIMProfileCache, error) {
	return profilecache.FindReadyShared(ctx, r.Client, namespace, profileName, scope)
}

// backingProfileNameFromRuntimeMeta extracts the backing AIMClusterProfile name
// from a managed runtime's metadata, preferring a controller ownerReference and
// falling back to the profile correlator label.
func backingProfileNameFromRuntimeMeta(ownerRefs []metav1.OwnerReference, labels map[string]string) string {
	for _, ref := range ownerRefs {
		if ref.Kind == "AIMClusterProfile" && ref.Name != "" {
			return ref.Name
		}
	}
	return labels[constants.LabelProfile]
}

func (r *InferenceServiceRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("aim-" + inferenceServiceRuntimeControllerName + "-controller")
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}

	// Index ISVCs by their referenced runtime name so the mapping handlers can
	// look up the affected consumers efficiently.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&servingv1beta1.InferenceService{},
		inferenceServiceRuntimeIndexKey,
		indexInferenceServiceRuntime,
	); err != nil {
		return err
	}

	// The materialized shadow objects are owned by the backing AIMClusterProfile,
	// not the InferenceService this controller reconciles, so `.Owns(...)` would
	// never fire on them. Instead, watch the inputs (managed shadow SR/ConfigMap,
	// backing profile, profile-owned cache) and map each change back to the
	// consuming ISVC(s) so the lazy shadow self-heals like the eager path does.
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&servingv1beta1.InferenceService{},
			builder.WithPredicates(inferenceServiceProjectionPredicate()),
		).
		Watches(
			&kservev1alpha1.ServingRuntime{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForManagedRuntimeObject),
			builder.WithPredicates(managedRuntimeObjectPredicate()),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForManagedRuntimeObject),
			builder.WithPredicates(managedRuntimeObjectPredicate()),
		).
		Watches(
			&aimv1alpha2.AIMClusterProfile{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForClusterProfile),
			builder.WithPredicates(clusterProfileProjectionPredicate()),
		).
		Watches(
			&aimv1alpha2.AIMProfile{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForProfile),
			builder.WithPredicates(profileProjectionPredicate()),
		).
		Watches(
			&aimv1alpha2.AIMProfileCache{},
			handler.EnqueueRequestsFromMapFunc(r.findInferenceServicesForProfileCache),
			builder.WithPredicates(profileCacheProjectionPredicate()),
		).
		Named(inferenceServiceRuntimeControllerName).
		Complete(r)
}

// indexInferenceServiceRuntime extracts the runtime an InferenceService
// references for the field index; ISVCs with no runtime reference are omitted.
func indexInferenceServiceRuntime(obj client.Object) []string {
	isvc, ok := obj.(*servingv1beta1.InferenceService)
	if !ok {
		return nil
	}
	name := runtimeprojection.ReferencedRuntimeName(isvc)
	if name == "" {
		return nil
	}
	return []string{name}
}

// findInferenceServicesForManagedRuntimeObject maps a changed managed shadow
// object (a ServingRuntime or its colocated ConfigMap under the reserved aim-
// prefix, labelled managed-by=aim-engine) back to the ISVCs in its namespace
// that reference its name. A hand-edit or delete of the shadow therefore
// re-triggers the authoritative force-apply that reasserts it.
func (r *InferenceServiceRuntimeReconciler) findInferenceServicesForManagedRuntimeObject(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	if !isManagedRuntimeObject(obj) {
		return nil
	}
	return r.inferenceServicesReferencingRuntimes(ctx, obj.GetNamespace(), []string{obj.GetName()})
}

// findInferenceServicesForClusterProfile maps a changed AIMClusterProfile to
// every InferenceService (cluster-wide) whose referenced runtime resolves to
// that profile, so backing-profile spec drift (image / resources / affinity)
// propagates into already-shadowed namespaces and a late-Ready profile
// materializes for an ISVC that raced ahead of it.
func (r *InferenceServiceRuntimeReconciler) findInferenceServicesForClusterProfile(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	profile, ok := obj.(*aimv1alpha2.AIMClusterProfile)
	if !ok {
		return nil
	}
	return r.inferenceServicesReferencingRuntimes(ctx, "", runtimeNamesForProfile(profile.Name, profile.Spec.AimId))
}

// findInferenceServicesForProfile maps a changed namespace AIMProfile to the
// InferenceServices in the profile's own namespace whose referenced runtime
// resolves to it, so backing-profile spec drift and a late-Ready namespace
// profile self-heal a namespace-profile-backed lazy shadow (the Reduced-mode
// case, where no eager per-profile runtime exists). Namespace-scoped, so the
// fan-out is confined to the profile's namespace — the only namespace whose
// shadows this profile can back.
func (r *InferenceServiceRuntimeReconciler) findInferenceServicesForProfile(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	profile, ok := obj.(*aimv1alpha2.AIMProfile)
	if !ok {
		return nil
	}
	return r.inferenceServicesReferencingRuntimes(ctx, profile.Namespace, runtimeNamesForProfile(profile.Name, profile.Spec.AimId))
}

// findInferenceServicesForProfileCache maps a changed profile-owned cache to the
// ISVCs in the cache's namespace whose runtime resolves to the cache's profile,
// so a cache reaching Ready after the ISVC exists gains its mount on the shadow
// without the ISVC being re-applied. Both scopes are relevant now that the lazy
// shadow completes namespace-profile-backed runtimes too: a cluster-scope cache
// backs a cross-scope AIMClusterProfile shadow, a namespace-scope cache backs a
// namespace-AIMProfile shadow. Either way the shadow and its cache live in the
// cache's namespace, so the fan-out is confined there.
//
// The cache carries only spec.profileName — never an aimId — so we resolve its
// backing profile to recover spec.aimId and fan out to BOTH reserved names the
// profile can back: the per-profile runtime AND the model-slug primary. Without
// this, a Reduced/Both model-slug consumer whose cache goes Ready after the
// shadow already exists would never be woken (the model-slug name is derived
// from the aimId the cache doesn't carry), leaving the profile-owned cache mount
// stranded until an unrelated reconcile. Resolution is best-effort — a mapping
// function cannot return an error — so a missing/unresolvable profile degrades
// to the per-profile fan-out (see aimIDForCache).
func (r *InferenceServiceRuntimeReconciler) findInferenceServicesForProfileCache(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	cache, ok := obj.(*aimv1alpha2.AIMProfileCache)
	if !ok {
		return nil
	}
	aimID := r.aimIDForCache(ctx, cache)
	return r.inferenceServicesReferencingRuntimes(ctx, cache.Namespace, runtimeNamesForProfile(cache.Spec.ProfileName, aimID))
}

// aimIDForCache recovers the spec.aimId of the profile a cache backs so the
// cache-event fan-out can also reach model-slug primary consumers, resolving the
// profile strictly at the cache's declared ProfileScope. It is best-effort by
// contract (a watch mapping function cannot return an error):
//
//   - profile resolves → its aimId (fans out to per-profile + model-slug names);
//   - profile NotFound  → "" (fans out to per-profile names only — no regression);
//   - any other lookup failure → logged, then "" (same safe degradation).
//
// An empty aimId is harmless downstream: runtimeNamesForProfile simply omits the
// model-slug name, and even a widened fan-out is filtered by DesiredFor, so the
// worst case of a stale/mismatched aimId is a redundant (dropped) reconcile.
func (r *InferenceServiceRuntimeReconciler) aimIDForCache(
	ctx context.Context,
	cache *aimv1alpha2.AIMProfileCache,
) string {
	profile, err := r.backingProfileForCache(ctx, cache)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve backing profile for cache event; falling back to per-profile fan-out",
			"cache", cache.Name, "namespace", cache.Namespace, "profile", cache.Spec.ProfileName)
		return ""
	}
	if profile == nil {
		return ""
	}
	return profile.GetProfileSpecCommon().AimId
}

// backingProfileForCache resolves the profile a cache caches, keyed on the
// cache's declared ProfileScope (the inverse of backingProfileScope): a
// cluster-scope cache resolves a cluster AIMClusterProfile; a namespace-scope
// cache (the default when the scope is empty, matching the profilecache
// resolver's normalization) resolves a namespace AIMProfile in the cache's own
// namespace. Resolving strictly by the declared scope — rather than the
// namespace-first lookupBackingProfile — avoids a cluster-scope cache picking up
// a same-named namespace profile (or vice versa). Returns a genuine nil
// interface (never a typed-nil pointer) when the profile is absent.
func (r *InferenceServiceRuntimeReconciler) backingProfileForCache(
	ctx context.Context,
	cache *aimv1alpha2.AIMProfileCache,
) (runtimeprojection.BackingProfile, error) {
	if cache.Spec.ProfileScope == aimv1alpha1.AIMResolutionScopeCluster {
		return r.clusterBackingProfile(ctx, cache.Spec.ProfileName)
	}
	return r.namespaceBackingProfile(ctx, cache.Namespace, cache.Spec.ProfileName)
}

// namespaceBackingProfile resolves a namespace AIMProfile by name in the given
// namespace, returning a genuine nil interface (not a typed-nil pointer) when
// absent. Unlike lookupBackingProfile it does not fall back to a cluster
// profile: callers that already know the scope (a namespace-scope cache) must
// not silently resolve a same-named cluster profile.
func (r *InferenceServiceRuntimeReconciler) namespaceBackingProfile(
	ctx context.Context,
	namespace, name string,
) (runtimeprojection.BackingProfile, error) {
	if name == "" {
		return nil, nil
	}
	var profile aimv1alpha2.AIMProfile
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &profile, nil
}

// inferenceServicesReferencingRuntimes lists, via the runtime field index, the
// ISVCs that reference any of runtimeNames and returns their reconcile requests,
// de-duplicated. An empty namespace widens the lookup cluster-wide (correct for
// cluster-scoped sources like AIMClusterProfile). List errors are logged and
// skipped so the fan-out is best-effort; the watch loop retries on the next
// event.
func (r *InferenceServiceRuntimeReconciler) inferenceServicesReferencingRuntimes(
	ctx context.Context,
	namespace string,
	runtimeNames []string,
) []reconcile.Request {
	requests := map[types.NamespacedName]struct{}{}
	for _, name := range runtimeNames {
		if name == "" {
			continue
		}
		opts := []client.ListOption{client.MatchingFields{inferenceServiceRuntimeIndexKey: name}}
		if namespace != "" {
			opts = append(opts, client.InNamespace(namespace))
		}
		var isvcs servingv1beta1.InferenceServiceList
		if err := r.List(ctx, &isvcs, opts...); err != nil {
			log.FromContext(ctx).Error(err, "failed to list InferenceServices referencing runtime",
				"runtime", name, "namespace", namespace)
			continue
		}
		for i := range isvcs.Items {
			requests[client.ObjectKeyFromObject(&isvcs.Items[i])] = struct{}{}
		}
	}

	out := make([]reconcile.Request, 0, len(requests))
	for nn := range requests {
		out = append(out, reconcile.Request{NamespacedName: nn})
	}
	return out
}

// runtimeNamesForProfile returns the reserved runtime names a profile can back:
// the per-profile name and, when the aimId is known, the model-slug primary
// name. Listing the slug name only widens the re-enqueue fan-out, which is
// harmless when DesiredFor declines it.
func runtimeNamesForProfile(profileName, aimID string) []string {
	names := []string{serving.RuntimeName(profileName)}
	if aimID != "" {
		names = append(names, serving.ModelSlugRuntimeName(aimID))
	}
	return names
}

// isManagedRuntimeObject reports whether an object is a shadow AIM Engine owns:
// its name is under the reserved aim- prefix and it carries the managed-by
// label. Both the ServingRuntime and its colocated ConfigMap qualify.
func isManagedRuntimeObject(obj client.Object) bool {
	return strings.HasPrefix(obj.GetName(), serving.RuntimeNamePrefix) &&
		obj.GetLabels()[constants.LabelK8sManagedBy] == constants.LabelValueManagedBy
}

// inferenceServiceProjectionPredicate drops ISVC updates that can't change the
// lazy shadow. The reconcile reads only the referenced runtime (spec) and the
// runtime-profile annotation, never status, so KServe's frequent status writes
// are pure churn — worst for the never-"complete" cross-scope/Reduced shadows
// that force-apply on every pass. Create/Delete/Generic keep the default (fire).
func inferenceServiceProjectionPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			// Spec changes bump generation; the runtime-profile annotation is
			// metadata (no bump) yet drives resolution, so check it too.
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			return e.ObjectOld.GetAnnotations()[constants.AnnotationRuntimeProfile] !=
				e.ObjectNew.GetAnnotations()[constants.AnnotationRuntimeProfile]
		},
	}
}

// managedRuntimeObjectPredicate restricts the SR/ConfigMap watches to managed
// shadow objects (aim- prefix + managed-by label) and to the events that
// warrant reasserting the force-apply: a drift update or a delete. The
// controller's own create is ignored — it already applied the object in the
// reconcile that produced it.
func managedRuntimeObjectPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return false },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isManagedRuntimeObject(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isManagedRuntimeObject(e.ObjectNew) },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}
}

// clusterProfileProjectionPredicate fires on the AIMClusterProfile changes that
// move what the shadow should contain: a spec change (image / resources /
// affinity source), a readiness transition (covers an ISVC that raced ahead of
// its profile), or a recomputed status.resources / status.resolvedNodeAffinity.
// A profile delete is ignored — ownerRef GC removes the shadow, and nothing can
// re-materialize it. Cosmetic status writes are filtered to avoid hot-loops.
func clusterProfileProjectionPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return false },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldProfile, ok1 := e.ObjectOld.(*aimv1alpha2.AIMClusterProfile)
			newProfile, ok2 := e.ObjectNew.(*aimv1alpha2.AIMClusterProfile)
			if !ok1 || !ok2 {
				return true
			}
			return profileProjectionChanged(
				oldProfile.Generation, newProfile.Generation,
				&oldProfile.Status, &newProfile.Status,
			)
		},
	}
}

// profileProjectionPredicate is the namespace-AIMProfile analogue of
// clusterProfileProjectionPredicate: it fires on the spec/status changes that
// move what a namespace-profile-backed lazy shadow should contain, and mirrors
// the cluster predicate's create/delete/generic policy (a delete is ignored —
// ownerRef GC removes the shadow and nothing re-materializes it).
func profileProjectionPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return false },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldProfile, ok1 := e.ObjectOld.(*aimv1alpha2.AIMProfile)
			newProfile, ok2 := e.ObjectNew.(*aimv1alpha2.AIMProfile)
			if !ok1 || !ok2 {
				return true
			}
			return profileProjectionChanged(
				oldProfile.Generation, newProfile.Generation,
				&oldProfile.Status, &newProfile.Status,
			)
		},
	}
}

// profileProjectionChanged reports whether a profile update moved something the
// lazy shadow reflects: a spec change (generation), a readiness/observed-
// generation transition (covers an ISVC that raced ahead of its profile), or a
// recomputed status.resources / status.resolvedNodeAffinity. Cosmetic status
// writes return false so the watch does not hot-loop. Shared by the cluster and
// namespace profile predicates since both scopes carry the same AIMProfileStatus.
func profileProjectionChanged(oldGen, newGen int64, oldStatus, newStatus *aimv1alpha2.AIMProfileStatus) bool {
	if oldGen != newGen {
		return true
	}
	if oldStatus.Status != newStatus.Status {
		return true
	}
	if oldStatus.ObservedGeneration != newStatus.ObservedGeneration {
		return true
	}
	if !equality.Semantic.DeepEqual(oldStatus.Resources, newStatus.Resources) {
		return true
	}
	return !equality.Semantic.DeepEqual(oldStatus.ResolvedNodeAffinity, newStatus.ResolvedNodeAffinity)
}

// profileCacheProjectionPredicate fires when a cache's readiness or resolved
// artifacts change — the signals that add or remove the profile-owned cache
// mount on the shadow (a late-Ready cache adds it; a delete reverts it).
// Cosmetic status writes are filtered to avoid hot-loops.
func profileCacheProjectionPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCache, ok1 := e.ObjectOld.(*aimv1alpha2.AIMProfileCache)
			newCache, ok2 := e.ObjectNew.(*aimv1alpha2.AIMProfileCache)
			if !ok1 || !ok2 {
				return true
			}
			if oldCache.Status.Status != newCache.Status.Status {
				return true
			}
			return !equality.Semantic.DeepEqual(oldCache.Status.Artifacts, newCache.Status.Artifacts)
		},
	}
}
