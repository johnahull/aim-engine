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

package serving

import (
	"strings"
	"testing"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	kserveconstants "github.com/kserve/kserve/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	"github.com/amd-enterprise-ai/aim-engine/internal/utils"
)

func gpuResources() *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			"amd.com/gpu":         resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
		},
		Limits: corev1.ResourceList{
			"amd.com/gpu": resource.MustParse("1"),
		},
	}
}

func mi300xAffinity() *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      "feature.node.kubernetes.io/aim-accelerator.MI300X",
							Operator: corev1.NodeSelectorOpExists,
						},
					},
				},
			},
		},
	}
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func findMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

// completeNoCacheRuntime builds the representative projectable, non-caching
// profile shared by the no-cache assertion tests.
func completeNoCacheRuntime(t *testing.T) (*kservev1alpha1.ServingRuntime, *corev1.ConfigMap) {
	t.Helper()
	spec := sampleSpec()
	spec.ContainerEnv = []corev1.EnvVar{{Name: "EXTRA", Value: "from-profile"}}

	runtime, cm, err := BuildNamespaceServingRuntime(NamespaceRuntimeInput{
		ProfileName:  "qwen3-32b-mi300x",
		Namespace:    "team-ml",
		Spec:         spec,
		Resources:    gpuResources(),
		NodeAffinity: mi300xAffinity(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return runtime, cm
}

// TestBuildNamespaceServingRuntime_MetadataAndLabels asserts the runtime is
// named via the hashed per-profile RuntimeName, scoped to the requested
// namespace, and carries the correlator + selectable labels (managed-by,
// accelerator-class, profile name, model slug, precision).
func TestBuildNamespaceServingRuntime_MetadataAndLabels(t *testing.T) {
	runtime, _ := completeNoCacheRuntime(t)

	if want := RuntimeName("qwen3-32b-mi300x"); runtime.Name != want {
		t.Errorf("runtime name = %q, want %q", runtime.Name, want)
	}
	if runtime.Namespace != "team-ml" {
		t.Errorf("runtime namespace = %q, want team-ml", runtime.Namespace)
	}
	if runtime.Labels[constants.LabelK8sManagedBy] != constants.LabelValueManagedBy {
		t.Errorf("managed-by label = %q", runtime.Labels[constants.LabelK8sManagedBy])
	}
	if runtime.Labels[constants.LabelKeyAcceleratorClass] != "MI300X" {
		t.Errorf("accelerator-class label = %q, want MI300X", runtime.Labels[constants.LabelKeyAcceleratorClass])
	}
	if runtime.Labels[constants.LabelProfile] != "qwen3-32b-mi300x" {
		t.Errorf("profile label = %q", runtime.Labels[constants.LabelProfile])
	}
	// Selectable model / precision labels: sanitized aimId slug and
	// lower-cased precision, so operators can filter runtimes by model/precision.
	if got, want := runtime.Labels[constants.LabelModelID], "qwen-qwen3-32b"; got != want {
		t.Errorf("model label = %q, want %q", got, want)
	}
	if got, want := runtime.Labels[constants.LabelPrecision], "fp8"; got != want {
		t.Errorf("precision label = %q, want %q", got, want)
	}
	// Projection-state marker: the runtime carries its own health signal so a
	// native KServe consumer need not hop to the backing profile's status.
	if got, want := runtime.Labels[constants.LabelRuntimeProjectionState], constants.LabelValueRuntimeProjectionStateProjected; got != want {
		t.Errorf("runtime-projection-state label = %q, want %q", got, want)
	}
}

// TestBuildNamespaceServingRuntime_FullFidelityAnnotations asserts the projected
// runtime and its colocated ConfigMap carry the full-fidelity identity
// annotations: the untruncated profile name plus aimId, modelId,
// precision, and metric — the readable counterpart to the opaque hashed name.
func TestBuildNamespaceServingRuntime_FullFidelityAnnotations(t *testing.T) {
	runtime, cm := completeNoCacheRuntime(t)

	want := map[string]string{
		constants.AnnotationProjectedProfile:   "qwen3-32b-mi300x",
		constants.AnnotationProjectedAimID:     "qwen/qwen3-32b",
		constants.AnnotationProjectedModelID:   "qwen/qwen3-32b-fp8",
		constants.AnnotationProjectedPrecision: "fp8",
		constants.AnnotationProjectedMetric:    "latency",
	}
	for k, v := range want {
		if got := runtime.Annotations[k]; got != v {
			t.Errorf("runtime annotation %q = %q, want %q", k, got, v)
		}
		if got := cm.Annotations[k]; got != v {
			t.Errorf("configMap annotation %q = %q, want %q", k, got, v)
		}
	}

	// The projection-state message companion is stamped on both objects, and the
	// colocated ConfigMap mirrors the runtime's projected-state label.
	if got := runtime.Annotations[constants.AnnotationRuntimeProjectionMessage]; got == "" {
		t.Error("runtime is missing the runtime-projection-message annotation")
	}
	if got := cm.Annotations[constants.AnnotationRuntimeProjectionMessage]; got == "" {
		t.Error("configMap is missing the runtime-projection-message annotation")
	}
	if got, want := cm.Labels[constants.LabelRuntimeProjectionState], constants.LabelValueRuntimeProjectionStateProjected; got != want {
		t.Errorf("configMap runtime-projection-state label = %q, want %q", got, want)
	}
}

// TestBuildNamespaceServingRuntime_ContainerAndFormat asserts the predictor
// container (image, resources), the supportedModelFormat/protocol, and the
// resolved node affinity.
func TestBuildNamespaceServingRuntime_ContainerAndFormat(t *testing.T) {
	runtime, _ := completeNoCacheRuntime(t)

	if len(runtime.Spec.Containers) != 1 {
		t.Fatalf("expected one container, got %d", len(runtime.Spec.Containers))
	}
	container := runtime.Spec.Containers[0]
	if container.Name != constants.ContainerKServe {
		t.Errorf("container name = %q, want %q", container.Name, constants.ContainerKServe)
	}
	if container.Image != sampleSpec().Image {
		t.Errorf("container image = %q, want %q", container.Image, sampleSpec().Image)
	}
	if got := container.Resources.Requests["amd.com/gpu"]; got.String() != "1" {
		t.Errorf("gpu request = %q, want 1", got.String())
	}

	if len(runtime.Spec.SupportedModelFormats) != 1 {
		t.Fatalf("expected one supported model format, got %d", len(runtime.Spec.SupportedModelFormats))
	}
	if format := runtime.Spec.SupportedModelFormats[0]; format.AutoSelect == nil || *format.AutoSelect {
		t.Errorf("autoSelect = %v, want false on a per-profile runtime", format.AutoSelect)
	}
	if len(runtime.Spec.ProtocolVersions) != 1 || runtime.Spec.ProtocolVersions[0] != kserveconstants.ProtocolV2 {
		t.Errorf("protocol versions = %v, want [v2]", runtime.Spec.ProtocolVersions)
	}
	if runtime.Spec.Affinity == nil || runtime.Spec.Affinity.NodeAffinity == nil {
		t.Fatalf("expected resolved node affinity on the runtime")
	}
}

// TestBuildNamespaceServingRuntime_ProfileConfigMapAndEnv asserts the colocated
// ConfigMap, its volume/mount wiring, and the standalone framework env (no cache
// redirect without modelSources).
func TestBuildNamespaceServingRuntime_ProfileConfigMapAndEnv(t *testing.T) {
	runtime, cm := completeNoCacheRuntime(t)
	container := runtime.Spec.Containers[0]

	if findMount(container.VolumeMounts, ProfileVolumePrefix) == nil {
		t.Errorf("missing profile ConfigMap mount %q", ProfileVolumePrefix)
	}
	for _, v := range runtime.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("unexpected PVC volume %q without caching", v.Name)
		}
	}

	env := envMap(container.Env)
	if env[constants.EnvAIMID] != sampleSpec().AimId {
		t.Errorf("AIM_ID = %q, want %q", env[constants.EnvAIMID], sampleSpec().AimId)
	}
	if _, ok := env[constants.EnvAIMModelID]; ok {
		t.Errorf("AIM_MODEL_ID must be unset without modelSources")
	}
	if env["EXTRA"] != "from-profile" {
		t.Errorf("profile container env not carried: %q", env["EXTRA"])
	}

	if cm == nil {
		t.Fatalf("expected a colocated ConfigMap")
	}
	if cm.Name != runtime.Name || cm.Namespace != runtime.Namespace {
		t.Errorf("ConfigMap name/namespace = %q/%q, want %q/%q", cm.Name, cm.Namespace, runtime.Name, runtime.Namespace)
	}
	if len(cm.Data) != 1 {
		t.Errorf("expected one profile YAML entry in ConfigMap, got %d", len(cm.Data))
	}
	vol := findVolume(runtime.Spec.Volumes, ProfileVolumePrefix)
	if vol == nil || vol.ConfigMap == nil || vol.ConfigMap.Name != cm.Name {
		t.Errorf("profile volume must reference the colocated ConfigMap %q", cm.Name)
	}
}

// TestBuildNamespaceServingRuntime_FrameworkEnvWinsOverProfile pins that the
// operator-managed AIM_* framework env wins over a colliding profile
// ContainerEnv: the identity-of-the-profile vars belong to the framework, not
// the profile author. The runtime now owns this precedence (the AIMService used
// to enforce it inline before Phase C moved the container onto the runtime).
func TestBuildNamespaceServingRuntime_FrameworkEnvWinsOverProfile(t *testing.T) {
	spec := sampleSpec()
	spec.ContainerEnv = []corev1.EnvVar{
		{Name: constants.EnvAIMProfileID, Value: "hijacked"},
		{Name: "PROFILE_ONLY", Value: "kept"},
	}

	runtime, _, err := BuildNamespaceServingRuntime(NamespaceRuntimeInput{
		ProfileName: "qwen3-32b-mi300x",
		Namespace:   "team-ml",
		Spec:        spec,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env := envMap(runtime.Spec.Containers[0].Env)
	if env[constants.EnvAIMProfileID] == "hijacked" {
		t.Errorf("profile ContainerEnv must not override framework %s", constants.EnvAIMProfileID)
	}
	if env[constants.EnvAIMProfileID] == "" {
		t.Errorf("framework env %s must be present", constants.EnvAIMProfileID)
	}
	if env["PROFILE_ONLY"] != "kept" {
		t.Errorf("non-colliding profile env must survive, got %q", env["PROFILE_ONLY"])
	}
}

// TestBuildNamespaceServingRuntime_WithProfileCache asserts the caching profile
// shape: the modelSources-conditional framework env redirects model loading and
// the runtime mounts the profile-owned cache PVC.
func TestBuildNamespaceServingRuntime_WithProfileCache(t *testing.T) {
	spec := sampleSpec()
	spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "qwen/qwen3-32b-fp8", SourceURI: "hf://qwen/qwen3-32b-fp8"}}

	cache := &aimv1alpha2.AIMProfileCache{
		Status: aimv1alpha2.AIMProfileCacheStatus{
			Status: constants.AIMStatusReady,
			Artifacts: map[string]aimv1alpha1.AIMResolvedArtifact{
				"weights": {
					Name:                  "qwen3-32b-fp8-weights",
					Model:                 "qwen/qwen3-32b-fp8",
					Status:                constants.AIMStatusReady,
					PersistentVolumeClaim: "pvc-qwen3-32b",
					MountPoint:            "/workspace/cache/qwen/qwen3-32b-fp8",
				},
			},
		},
	}

	runtime, _, err := BuildNamespaceServingRuntime(NamespaceRuntimeInput{
		ProfileName:  "qwen3-32b-mi300x",
		Namespace:    "team-ml",
		Spec:         spec,
		Resources:    gpuResources(),
		NodeAffinity: mi300xAffinity(),
		Cache:        cache,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	container := runtime.Spec.Containers[0]

	// Cache-redirect framework env.
	env := envMap(container.Env)
	if env[constants.EnvAIMID] != "" {
		t.Errorf("AIM_ID must be cleared when redirecting, got %q", env[constants.EnvAIMID])
	}
	if env[constants.EnvAIMModelID] != "qwen/qwen3-32b-fp8" {
		t.Errorf("AIM_MODEL_ID = %q", env[constants.EnvAIMModelID])
	}
	if env[constants.EnvAIMCachePath] != constants.AIMCacheBasePath {
		t.Errorf("AIM_CACHE_PATH = %q, want %q", env[constants.EnvAIMCachePath], constants.AIMCacheBasePath)
	}

	// Profile-owned cache PVC volume + mount.
	volName := "qwen3-32b-fp8-weights"
	vol := findVolume(runtime.Spec.Volumes, volName)
	if vol == nil || vol.PersistentVolumeClaim == nil {
		t.Fatalf("missing profile cache PVC volume %q", volName)
	}
	if vol.PersistentVolumeClaim.ClaimName != "pvc-qwen3-32b" {
		t.Errorf("cache PVC claim = %q, want pvc-qwen3-32b", vol.PersistentVolumeClaim.ClaimName)
	}
	mount := findMount(container.VolumeMounts, volName)
	if mount == nil || mount.MountPath != "/workspace/cache/qwen/qwen3-32b-fp8" {
		t.Fatalf("missing or wrong profile cache mount: %+v", mount)
	}
}

// TestBuildNamespaceServingRuntime_CacheNotReadyOmitsMount guards that a profile
// opting into caching whose cache is not yet Ready does not get a PVC mount (the
// consumer defers; the runtime stays mountless until the cache resolves).
func TestBuildNamespaceServingRuntime_CacheNotReadyOmitsMount(t *testing.T) {
	spec := sampleSpec()
	spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "qwen/qwen3-32b-fp8", SourceURI: "hf://qwen/qwen3-32b-fp8"}}

	cache := &aimv1alpha2.AIMProfileCache{
		Status: aimv1alpha2.AIMProfileCacheStatus{Status: constants.AIMStatusProgressing},
	}

	runtime, _, err := BuildNamespaceServingRuntime(NamespaceRuntimeInput{
		ProfileName: "qwen3-32b-mi300x",
		Namespace:   "team-ml",
		Spec:        spec,
		Cache:       cache,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, v := range runtime.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("unexpected PVC volume %q while cache not ready", v.Name)
		}
	}
}

// TestRuntimeName_FormatBoundsAndDeterminism pins the per-profile runtime name
// contract: every result stays under the reserved aim- prefix, is
// always ≤63 chars even for a pathologically long profile name, carries a
// hash suffix, and is deterministic (a pure func of the profile name — every
// symmetric caller recomputes the identical name).
func TestRuntimeName_FormatBoundsAndDeterminism(t *testing.T) {
	cases := []struct {
		name    string
		profile string
	}{
		{"short", "p1"},
		{"typical", "qwen3-32b-mi300x-fp8-tp1-latency"},
		{"at-ish limit", strings.Repeat("a", 55)},
		{"overflow", strings.Repeat("very-long-profile-name-", 10)},
		{"unicode / invalid chars", "Qwen/Qwen3_32B::MI300X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RuntimeName(tc.profile)

			if !strings.HasPrefix(got, RuntimeNamePrefix) {
				t.Errorf("RuntimeName(%q) = %q, must keep the reserved %q prefix", tc.profile, got, RuntimeNamePrefix)
			}
			if len(got) > utils.MaxKubernetesNameLength {
				t.Errorf("RuntimeName(%q) = %q (len %d) exceeds %d", tc.profile, got, len(got), utils.MaxKubernetesNameLength)
			}
			// A hash suffix is always appended (aim-<part>-<hash>), so there are
			// at least two hyphen-separated segments after the stem.
			if segs := strings.Split(got, "-"); len(segs) < 3 {
				t.Errorf("RuntimeName(%q) = %q, expected a hash suffix segment", tc.profile, got)
			}
			if again := RuntimeName(tc.profile); again != got {
				t.Errorf("RuntimeName(%q) not deterministic: %q != %q", tc.profile, again, got)
			}
		})
	}
}

// TestRuntimeName_NoCollisions checks that distinct profile names never collapse
// to the same per-profile runtime name — including two long names that share the
// same 63-char-truncated prefix but differ in the tail, where the hash suffix is
// what keeps them apart.
func TestRuntimeName_NoCollisions(t *testing.T) {
	longA := strings.Repeat("collision-", 8) + "aaa"
	longB := strings.Repeat("collision-", 8) + "bbb"

	names := []string{"p1", "p2", "qwen3-32b", "qwen3-32b-fp8", longA, longB}
	seen := map[string]string{}
	for _, profile := range names {
		got := RuntimeName(profile)
		if prev, ok := seen[got]; ok {
			t.Fatalf("RuntimeName collision: %q and %q both produced %q", prev, profile, got)
		}
		seen[got] = profile
	}

	// The two long names truncate to the same readable prefix; only the hash
	// suffix distinguishes them.
	if RuntimeName(longA) == RuntimeName(longB) {
		t.Errorf("expected differing hashes for %q vs %q", longA, longB)
	}
}

// TestModelSlugRuntimeName_ReadableAndBounded pins the model-slug primary name
// contract: it stays readable (never hashed) — a plain
// aim-<model-slug> — and is truncated (not hashed) to respect the 63-char limit
// on the rare overflow.
func TestModelSlugRuntimeName_ReadableAndBounded(t *testing.T) {
	if got, want := ModelSlugRuntimeName("qwen/qwen3-32b"), "aim-qwen-qwen3-32b"; got != want {
		t.Errorf("ModelSlugRuntimeName = %q, want %q", got, want)
	}

	// Overflow: a very long aimId is truncated but NOT hashed — the result stays
	// a readable slug under the prefix and within the length limit.
	long := "org/" + strings.Repeat("z", 120)
	got := ModelSlugRuntimeName(long)
	if !strings.HasPrefix(got, RuntimeNamePrefix) {
		t.Errorf("ModelSlugRuntimeName(%q) = %q, want %q prefix", long, got, RuntimeNamePrefix)
	}
	if len(got) > utils.MaxKubernetesNameLength {
		t.Errorf("ModelSlugRuntimeName(%q) = %q (len %d) exceeds %d", long, got, len(got), utils.MaxKubernetesNameLength)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("ModelSlugRuntimeName(%q) = %q must not end in a hyphen", long, got)
	}
	// Readability: the slug body is exactly the (truncated) model slug, with no
	// hash suffix appended.
	slug := strings.TrimPrefix(got, RuntimeNamePrefix)
	if !strings.HasPrefix(ModelSlug(long), slug) {
		t.Errorf("ModelSlugRuntimeName(%q) body %q is not a readable prefix of the slug %q", long, slug, ModelSlug(long))
	}
}

// bareCSRProfileID returns the AIM_PROFILE_ID env value on a bare
// ClusterServingRuntime's predictor container.
func bareCSRProfileID(t *testing.T, csr *kservev1alpha1.ClusterServingRuntime) string {
	t.Helper()
	return envMap(csr.Spec.Containers[0].Env)[constants.EnvAIMProfileID]
}

// TestBareCSRProfileID_MatchesShadowConfigMapKey pins the invariant: the
// AIM_PROFILE_ID the eager bare ClusterServingRuntime carries resolves against the
// data KEY of the ConfigMap the lazy/eager complete namespace runtime
// materializes — so once the shadow lands the env never dangles. Both are derived
// from the same ProfileFilename (the profile's identity axes), so the round-trip
// holds for cache (modelSources) and no-cache profiles alike.
func TestBareCSRProfileID_MatchesShadowConfigMapKey(t *testing.T) {
	cases := []struct {
		name         string
		modelSources bool
	}{
		{"no-cache profile", false},
		{"cache (modelSources) profile", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := sampleSpec()
			if tc.modelSources {
				spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}
			}

			// The bare CSR has no ConfigMap of its own; it only carries the env.
			csr, err := BuildClusterServingRuntime(ClusterRuntimeInput{
				ProfileName: "qwen3-32b-mi300x",
				Spec:        spec,
			})
			if err != nil {
				t.Fatalf("BuildClusterServingRuntime: %v", err)
			}
			// The complete namespace runtime is the shadow that completes it: it
			// carries the colocated ConfigMap the env resolves against.
			runtime, cm, err := BuildNamespaceServingRuntime(NamespaceRuntimeInput{
				ProfileName: "qwen3-32b-mi300x",
				Namespace:   "team-ml",
				Spec:        spec,
			})
			if err != nil {
				t.Fatalf("BuildNamespaceServingRuntime: %v", err)
			}

			if len(cm.Data) != 1 {
				t.Fatalf("expected one shadow ConfigMap data key, got %d", len(cm.Data))
			}
			var key string
			for k := range cm.Data {
				key = k
			}

			bareID := bareCSRProfileID(t, csr)
			wantID := "custom/" + spec.AimId + "/" + strings.TrimSuffix(key, ".yaml")
			if bareID != wantID {
				t.Errorf("bare CSR AIM_PROFILE_ID = %q, want %q (must resolve to ConfigMap key %q)", bareID, wantID, key)
			}

			// The complete namespace runtime that mounts the ConfigMap sets the
			// identical env, so what the bare CSR references is exactly what the
			// shadow satisfies.
			shadowID := envMap(runtime.Spec.Containers[0].Env)[constants.EnvAIMProfileID]
			if shadowID != bareID {
				t.Errorf("shadow AIM_PROFILE_ID = %q, want %q (must match the bare CSR)", shadowID, bareID)
			}

			// Path resolution: the ConfigMap is mounted at <ProfileMountBase>/<aimId>
			// with the key <name>.yaml, so the runtime finds the file
			// AIM_PROFILE_ID (custom/<aimId>/<name>) names.
			mount := findMount(runtime.Spec.Containers[0].VolumeMounts, ProfileVolumePrefix)
			if mount == nil {
				t.Fatalf("shadow runtime missing profile ConfigMap mount")
			}
			gotFilePath := mount.MountPath + "/" + key
			wantFilePath := ProfileMountBase + "/" + strings.TrimPrefix(bareID, "custom/") + ".yaml"
			if gotFilePath != wantFilePath {
				t.Errorf("mounted profile file %q does not match AIM_PROFILE_ID resolution %q", gotFilePath, wantFilePath)
			}
		})
	}
}

// TestBareCSRProfileID_IndependentOfRuntimeObjectName pins the invariant:
// AIM_PROFILE_ID is keyed on the profile's identity axes (via
// ProfileFilename), NOT the KServe runtime OBJECT name, so hashing or overriding
// the runtime object name does not change it. Renaming/hashing the runtime object
// can therefore never make the bare-CSR env dangle against a differently-keyed
// shadow ConfigMap.
func TestBareCSRProfileID_IndependentOfRuntimeObjectName(t *testing.T) {
	spec := sampleSpec()

	// Default per-profile object name: aim-<truncated>-<hash>.
	hashed, err := BuildClusterServingRuntime(ClusterRuntimeInput{
		ProfileName: "qwen3-32b-mi300x",
		Spec:        spec,
	})
	if err != nil {
		t.Fatalf("BuildClusterServingRuntime (hashed): %v", err)
	}
	// A different object name (the model-slug primary override) for the SAME
	// profile spec.
	slug, err := BuildClusterServingRuntime(ClusterRuntimeInput{
		ProfileName: "qwen3-32b-mi300x",
		Name:        ModelSlugRuntimeName(spec.AimId),
		Spec:        spec,
	})
	if err != nil {
		t.Fatalf("BuildClusterServingRuntime (slug): %v", err)
	}

	if hashed.Name == slug.Name {
		t.Fatalf("expected distinct runtime object names, both = %q", hashed.Name)
	}
	if got, want := bareCSRProfileID(t, hashed), bareCSRProfileID(t, slug); got != want {
		t.Errorf("AIM_PROFILE_ID depends on the runtime object name: %q (%s) != %q (%s)",
			got, hashed.Name, want, slug.Name)
	}
}

// TestBuildClusterServingRuntime_ProjectionStateMarker asserts the bare CSR — the
// cluster-scoped, natively-referenceable BYO handle — carries the projected-state
// marker and its message companion on the object itself, so a native consumer
// inspecting the ClusterServingRuntime sees projection state without resolving the
// backing profile.
func TestBuildClusterServingRuntime_ProjectionStateMarker(t *testing.T) {
	csr, err := BuildClusterServingRuntime(ClusterRuntimeInput{
		ProfileName: "qwen3-32b-mi300x",
		Spec:        sampleSpec(),
	})
	if err != nil {
		t.Fatalf("BuildClusterServingRuntime: %v", err)
	}
	if got, want := csr.Labels[constants.LabelRuntimeProjectionState], constants.LabelValueRuntimeProjectionStateProjected; got != want {
		t.Errorf("CSR runtime-projection-state label = %q, want %q", got, want)
	}
	if got := csr.Annotations[constants.AnnotationRuntimeProjectionMessage]; got == "" {
		t.Error("CSR is missing the runtime-projection-message annotation")
	}
}

func TestBuildNamespaceServingRuntime_InvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		input NamespaceRuntimeInput
	}{
		{"nil spec", NamespaceRuntimeInput{ProfileName: "p", Namespace: "ns"}},
		{"empty name", NamespaceRuntimeInput{Namespace: "ns", Spec: sampleSpec()}},
		{"empty namespace", NamespaceRuntimeInput{ProfileName: "p", Spec: sampleSpec()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := BuildNamespaceServingRuntime(tc.input); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
