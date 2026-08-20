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

// Package runtimeprojection holds the lazy InferenceService-watch runtime
// projection: when a KServe InferenceService references a managed runtime by
// name that does not already resolve to a complete runtime in the ISVC's
// namespace, AIM Engine materializes a complete namespace ServingRuntime (+
// colocated profile ConfigMap) in that namespace, owned by the backing
// AIMClusterProfile. This shadows a bare ClusterServingRuntime with a complete
// namespace ServingRuntime (the CSR stays standalone-usable elsewhere).
//
// DesiredFor is the pure seam: given an InferenceService and the resolved
// cluster state, it decides what (if anything) to materialize. The controller
// wiring fetches that state and applies the result authoritatively.
package runtimeprojection

import (
	"fmt"
	"strings"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	servingv1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/profileyaml"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

// BackingProfile is the scope-agnostic view of the profile that backs a
// referenced runtime: either a cluster AIMClusterProfile or a namespace
// AIMProfile. Both embed AIMProfileSpecCommon and AIMProfileStatus and are
// client.Objects, which is everything the lazy projection needs to build and
// own a complete namespace ServingRuntime — so resolution can settle on either
// scope and the shared builder handles the rest.
type BackingProfile interface {
	client.Object
	GetProfileSpecCommon() aimv1alpha2.AIMProfileSpecCommon
	GetStatus() *aimv1alpha2.AIMProfileStatus
}

// ProjectionState carries the resolved cluster state the pure DesiredFor seam
// needs to decide whether (and what) to materialize for an InferenceService.
//
// The two profile candidates set the backing-profile resolution order: a managed
// ClusterServingRuntime's backing profile (the native, annotation-free flow)
// wins; the AIMService-stamped annotation is the cross-scope fast-path for a
// runtime not yet created. DesiredFor picks the first non-nil. There is no
// reverse name-lookup candidate because hashed per-profile names aren't
// reversible, so the ownerRef and the annotation are the only resolution paths.
//
// The annotation candidate carries either scope (namespace AIMProfile or cluster
// AIMClusterProfile, resolved namespace-first), which lets a Reduced-mode
// namespace-profile-backed service self-complete with no eager per-profile runtime.
type ProjectionState struct {
	// NamespaceRuntimeComplete reports that the referenced runtime already
	// resolves to a complete namespace runtime (ServingRuntime + colocated
	// ConfigMap) that the lazy projection must defer to — e.g. a same-namespace
	// namespace profile's eager projection, or a hand-authored runtime. A prior
	// lazy shadow does NOT set this: it must be re-applied so drift, a late
	// cache, or backing-profile changes are reasserted. When true, no
	// materialization is needed.
	NamespaceRuntimeComplete bool

	// ManagedRuntimeProfile is the profile resolved by following the referenced
	// managed ClusterServingRuntime's ownerRef/label (resolution step 1, the
	// native annotation-free flow). Always an AIMClusterProfile in practice (a
	// namespace profile projects a namespace ServingRuntime, not a CSR). Nil when
	// the referenced runtime is not a managed ClusterServingRuntime.
	ManagedRuntimeProfile BackingProfile

	// AnnotatedProfile is the profile named by the AIMService-stamped
	// runtime-profile annotation on the ISVC (resolution step 2 fast-path, used
	// when the runtime is not yet created). A namespace AIMProfile in the ISVC's
	// namespace or a cluster AIMClusterProfile. Nil when absent.
	AnnotatedProfile BackingProfile

	// Cache is the ready profile-owned AIMProfileCache colocated in the ISVC's
	// namespace, when the backing profile opts into caching. Nil otherwise; it
	// contributes the profile-owned cache mount on the materialized runtime.
	Cache *aimv1alpha2.AIMProfileCache
}

// BackingProfile returns the profile the referenced runtime resolves to,
// applying the resolution order: managed ClusterServingRuntime first, then the
// AIMService-stamped annotation fast-path. Returns nil when neither resolves.
// The candidates carry genuine nil interfaces (not typed-nil pointers), so the
// first-non-nil switch is safe. There is no name-lookup fallback because hashed
// per-profile runtime names are not reversible.
func (s ProjectionState) BackingProfile() BackingProfile {
	switch {
	case s.ManagedRuntimeProfile != nil:
		return s.ManagedRuntimeProfile
	default:
		return s.AnnotatedProfile
	}
}

// DesiredProjection is what DesiredFor decides to materialize. A zero value
// (nil Runtime) means "nothing to do".
type DesiredProjection struct {
	// Runtime is the complete namespace ServingRuntime to apply, or nil.
	Runtime *kservev1alpha1.ServingRuntime
	// ConfigMap is the colocated profile ConfigMap to apply, or nil.
	ConfigMap *corev1.ConfigMap
	// Owner is the backing profile (cluster or namespace scope) that owns the
	// materialized objects, so they are garbage collected when the profile is
	// deleted. Nil when nothing is materialized.
	Owner BackingProfile
}

// ReferencedRuntimeName returns the runtime an InferenceService explicitly
// references via predictor.model.runtime, or "" when none is set.
func ReferencedRuntimeName(isvc *servingv1beta1.InferenceService) string {
	model := isvc.Spec.Predictor.Model
	if model == nil || model.Runtime == nil {
		return ""
	}
	return *model.Runtime
}

// DesiredFor decides what to materialize for an InferenceService given the
// resolved cluster state. It returns nothing (a zero DesiredProjection) unless
// the ISVC references a runtime under the reserved aim- prefix that does not yet
// resolve to a complete namespace runtime AND a backing AIMClusterProfile
// resolves for it. In that case it returns a complete namespace ServingRuntime
// (+ colocated ConfigMap) owned by that cluster profile.
//
// Names outside the reserved aim- prefix are left alone: AIM Engine never reads,
// creates, or modifies runtimes it does not own.
func DesiredFor(isvc *servingv1beta1.InferenceService, state ProjectionState) (DesiredProjection, error) {
	runtimeName := ReferencedRuntimeName(isvc)
	if runtimeName == "" {
		return DesiredProjection{}, nil
	}

	// Only ever touch names under the reserved aim- prefix; any other name is a
	// hand-authored runtime AIM Engine must not shadow.
	if !strings.HasPrefix(runtimeName, serving.RuntimeNamePrefix) {
		return DesiredProjection{}, nil
	}

	// The referenced runtime already resolves to a complete namespace runtime
	// here (same-namespace namespace profile, or a prior shadow) — nothing to do.
	if state.NamespaceRuntimeComplete {
		return DesiredProjection{}, nil
	}

	profile := state.BackingProfile()
	if profile == nil {
		return DesiredProjection{}, nil
	}

	// Only shadow a runtime name the resolved profile actually projects. A
	// mismatch means the reference does not belong to this profile, so
	// materializing under runtimeName would not shadow it.
	if !runtimeNameMatchesProfile(profile, runtimeName) {
		return DesiredProjection{}, nil
	}

	spec := profile.GetProfileSpecCommon()
	status := profile.GetStatus()
	origin := profileyaml.EffectiveOrigin(profile.GetLabels(), status.Origin)
	yamlContract, err := profileyaml.ForProfile(profile.GetAnnotations(), origin, &spec)
	if err != nil {
		return DesiredProjection{}, fmt.Errorf("parse profile YAML contract for profile %q: %w", profile.GetName(), err)
	}
	input := serving.NamespaceRuntimeInput{
		ProfileName:  profile.GetName(),
		Namespace:    isvc.Namespace,
		Spec:         &spec,
		YAMLContract: yamlContract,
		Resources:    status.Resources,
		NodeAffinity: status.ResolvedNodeAffinity,
		Cache:        state.Cache,
	}

	// A reference that matched via the model-slug name (not the per-profile
	// name) completes the Reduced/Both-mode model-slug primary: materialize the
	// shadow under the referenced slug name, mirroring the eager slug runtime
	// (aimprofile.planNamespaceModelSlugRuntime). autoSelect stays off on every
	// projected runtime (the shared model format would otherwise collide across
	// models in KServe auto-selection); native consumers reference the runtime
	// by name. The correlator label still points to the backing primary profile
	// (ProfileName is unchanged).
	if runtimeName != serving.RuntimeName(profile.GetName()) && matchesModelSlugRuntime(profile, runtimeName) {
		input.Name = runtimeName
	}

	runtime, configMap, err := serving.BuildNamespaceServingRuntime(input)
	if err != nil {
		return DesiredProjection{}, fmt.Errorf("build namespace serving runtime for profile %q: %w", profile.GetName(), err)
	}

	// Mark both objects as lazily materialized so the controller can tell this
	// shadow apart from an eager per-profile projection of the same name (they
	// otherwise share the AIMProfile ownerReference for a namespace profile) and
	// re-apply it on every reconcile to self-heal.
	markLazyProjection(runtime)
	markLazyProjection(configMap)

	return DesiredProjection{Runtime: runtime, ConfigMap: configMap, Owner: profile}, nil
}

// markLazyProjection stamps the lazy-projection marker label on a materialized
// shadow object, so the controller re-applies it (self-heal) rather than
// deferring to it as if it were an eager projection or hand-authored runtime.
func markLazyProjection(obj metav1.Object) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[constants.LabelRuntimeProjection] = constants.LabelValueRuntimeProjectionLazy
	obj.SetLabels(labels)
}

// runtimeNameMatchesProfile reports whether runtimeName is a reserved runtime
// name the resolved profile actually projects. Two names qualify: the
// per-profile name (aim-<profile.Name>), and the Reduced/Both-mode model-slug
// primary name (aim-<model-slug>, derived from the profile's aimId). A native
// ISVC referencing either resolves to this profile, so both must be shadowed
// cross-namespace — the slug is the portable, vendor-independent handle the
// eager slug projection publishes (aimprofile.planClusterModelSlugRuntime).
func runtimeNameMatchesProfile(profile BackingProfile, runtimeName string) bool {
	if serving.RuntimeName(profile.GetName()) == runtimeName {
		return true
	}
	return matchesModelSlugRuntime(profile, runtimeName)
}

// matchesModelSlugRuntime reports whether runtimeName is the model-slug primary
// runtime name for the profile's aimId. It mirrors the eager slug projection's
// own guard (aimprofile.modelSlugProjectable), which publishes a slug runtime
// only for a profile carrying an aimId — so an empty aimId never matches (it
// would otherwise collapse to the bare "aim-" prefix and shadow an unrelated
// reference).
func matchesModelSlugRuntime(profile BackingProfile, runtimeName string) bool {
	aimID := profile.GetProfileSpecCommon().AimId
	if aimID == "" {
		return false
	}
	return serving.ModelSlugRuntimeName(aimID) == runtimeName
}
