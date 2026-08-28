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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/runtimeprojection"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

// TestEagerProjection_MountsServiceDrivenSharedCache is the regression guard for
// the mode-dependent Shared-cache mount: a projectable namespace profile that
// itself has caching DISABLED, but for which an AIMService created a Ready Shared
// AIMProfileCache (hashed name, not the profile name), must still have that cache
// mounted on the eagerly-projected per-profile ServingRuntime. Before the fix the
// eager path only looked up a cache named after the profile gated on
// caching.enabled, so the Shared cache went unmounted from the eagerly projected
// per-profile runtime and the pod cold-pulled its weights.
func TestEagerProjection_MountsServiceDrivenSharedCache(t *testing.T) {
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-cpu", Namespace: "team-a"},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{
				AimId: "qwen/qwen3-32b",
				Image: "registry.io/qwen3-32b:1.0.0",
				ModelSources: []aimv1alpha1.AIMModelSource{
					{ModelID: "qwen/qwen3-32b", SourceURI: "hf://qwen/qwen3-32b"},
				},
			},
			// caching is intentionally NOT enabled on the profile.
		},
	}

	const claimName = "qwen3-32b-cache-pvc"
	// The service-driven Shared cache: hashed object name (as GenerateProfileCacheName
	// produces), spec.profileName pointing back at the profile, Shared + Ready.
	serviceCache := &aimv1alpha2.AIMProfileCache{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen3-32b-cpu-cache-fe2d5ebc", Namespace: "team-a"},
		Spec: aimv1alpha2.AIMProfileCacheSpec{
			ProfileName:  profile.Name,
			ProfileScope: aimv1alpha1.AIMResolutionScopeNamespace,
			Mode:         aimv1alpha2.ProfileCacheModeShared,
		},
		Status: aimv1alpha2.AIMProfileCacheStatus{
			Status: constants.AIMStatusReady,
			Artifacts: map[string]aimv1alpha1.AIMResolvedArtifact{
				"qwen3-32b": {
					Name:                  "qwen3-32b",
					Model:                 "qwen/qwen3-32b",
					Status:                constants.AIMStatusReady,
					PersistentVolumeClaim: claimName,
					MountPoint:            "/workspace/cache/qwen3-32b",
				},
			},
		},
	}

	c := newProfileTestClient(t, profile, serviceCache)
	r := &ProfileReconciler{Client: c, ProjectionMode: aimv1alpha2.RuntimeProjectionModeExhaustive}
	reconcileCtx := controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}

	fetch := r.FetchRemoteState(context.Background(), c, reconcileCtx)
	if fetch.profileCache == nil {
		t.Fatalf("FetchRemoteState did not resolve the service-driven Shared cache")
	}
	obs := r.ComposeState(context.Background(), reconcileCtx, fetch)
	if !obs.projectable {
		t.Fatalf("profile should be projectable (deployable CPU profile with image)")
	}

	plan := r.PlanResources(context.Background(), reconcileCtx, obs)
	sr := findServingRuntime(t, plan.GetToApplyWithForce())

	if !runtimeHasPVCVolume(sr.Spec.Volumes, claimName) {
		t.Fatalf("projected runtime must mount the service-driven Shared cache PVC %q; volumes=%+v", claimName, sr.Spec.Volumes)
	}
	if !containerMountsCache(sr.Spec.Containers[0].VolumeMounts, "/workspace/cache/qwen3-32b") {
		t.Fatalf("projected runtime container must mount the cache at its MountPoint; mounts=%+v", sr.Spec.Containers[0].VolumeMounts)
	}
	// The cache-redirect framework env must accompany the mount.
	if !hasEnv(sr.Spec.Containers[0].Env, constants.EnvAIMCachePath) {
		t.Fatalf("projected runtime missing %s env for cache redirect", constants.EnvAIMCachePath)
	}
}

func TestRetainedReducedProjection_MatchesConsumerHashWithSharedCache(t *testing.T) {
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "retained-cpu", Namespace: "team-a"},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: aimv1alpha2.AIMProfileSpecCommon{
				AimId: "qwen/qwen3-32b",
				Image: "registry.io/qwen3-32b:2.0.0",
				ModelSources: []aimv1alpha1.AIMModelSource{
					{ModelID: "qwen/qwen3-32b", SourceURI: "hf://qwen/qwen3-32b"},
				},
			},
		},
	}
	profile.Status.ProjectedRuntimeName = serving.RuntimeName(profile.Name)
	cache := &aimv1alpha2.AIMProfileCache{
		ObjectMeta: metav1.ObjectMeta{Name: "a-retained-shared-cache", Namespace: profile.Namespace},
		Spec: aimv1alpha2.AIMProfileCacheSpec{
			ProfileName:  profile.Name,
			ProfileScope: aimv1alpha1.AIMResolutionScopeNamespace,
			Mode:         aimv1alpha2.ProfileCacheModeShared,
		},
		Status: aimv1alpha2.AIMProfileCacheStatus{
			Status: constants.AIMStatusReady,
			Artifacts: map[string]aimv1alpha1.AIMResolvedArtifact{
				"weights": {
					Name:                  "weights",
					Model:                 "qwen/qwen3-32b",
					Status:                constants.AIMStatusReady,
					PersistentVolumeClaim: "retained-cache-pvc",
					MountPoint:            "/workspace/cache/qwen3-32b",
				},
			},
		},
	}

	c := newProfileTestClient(t, profile, cache)
	r := &ProfileReconciler{Client: c, ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
	reconcileCtx := controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: profile}
	fetch := r.FetchRemoteState(context.Background(), c, reconcileCtx)
	obs := r.ComposeState(context.Background(), reconcileCtx, fetch)
	if obs.projectionErr != nil {
		t.Fatalf("compose retained Reduced projection: %v", obs.projectionErr)
	}

	plan := r.PlanResources(context.Background(), reconcileCtx, obs)
	eagerRuntime := findServingRuntime(t, plan.GetToApplyWithForce())
	eagerConfigMap := findConfigMap(t, plan.GetToApplyWithForce(), eagerRuntime.Name, profile.Namespace)
	if !runtimeHasPVCVolume(eagerRuntime.Spec.Volumes, "retained-cache-pvc") {
		t.Fatalf("retained Reduced projection did not select current Shared cache: %+v", eagerRuntime.Spec.Volumes)
	}

	profile.Status.Resources = obs.resolvedResources
	profile.Status.ResolvedNodeAffinity = obs.matchResult.NodeAffinity
	profile.Status.Origin = obs.origin
	desired, err := runtimeprojection.DesiredForRuntime(
		profile.Namespace,
		eagerRuntime.Name,
		runtimeprojection.ProjectionState{AnnotatedProfile: profile, Cache: cache},
	)
	if err != nil {
		t.Fatalf("build consumer projection: %v", err)
	}
	wantHash := desired.Runtime.Annotations[constants.AnnotationRuntimeProjectionContentHash]
	if got := eagerRuntime.Annotations[constants.AnnotationRuntimeProjectionContentHash]; got != wantHash {
		t.Fatalf("retained Reduced runtime hash = %q, consumer wants %q", got, wantHash)
	}
	if got := eagerConfigMap.Annotations[constants.AnnotationRuntimeProjectionContentHash]; got != wantHash {
		t.Fatalf("retained Reduced ConfigMap hash = %q, consumer wants %q", got, wantHash)
	}
}

func runtimeHasPVCVolume(volumes []corev1.Volume, claimName string) bool {
	for _, v := range volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

func containerMountsCache(mounts []corev1.VolumeMount, mountPath string) bool {
	for _, m := range mounts {
		if m.MountPath == mountPath {
			return true
		}
	}
	return false
}

func newProfileTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		aimv1alpha1.AddToScheme,
		aimv1alpha2.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("AddToScheme() error = %v", err)
		}
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&aimv1alpha2.AIMProfile{}, aimv1alpha2.ProfileAimIdIndexKey, func(obj client.Object) []string {
			profile := obj.(*aimv1alpha2.AIMProfile)
			if profile.Spec.AimId == "" {
				return nil
			}
			return []string{profile.Spec.AimId}
		}).
		WithIndex(&aimv1alpha2.AIMClusterProfile{}, aimv1alpha2.ProfileAimIdIndexKey, func(obj client.Object) []string {
			profile := obj.(*aimv1alpha2.AIMClusterProfile)
			if profile.Spec.AimId == "" {
				return nil
			}
			return []string{profile.Spec.AimId}
		}).
		Build()
}
