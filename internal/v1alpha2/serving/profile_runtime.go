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

// Package serving holds the profile→serving building blocks shared by the
// profile reconcilers (which project KServe runtimes) and the AIMService
// reconciler (which consumes them). Everything here is purely profile-derived,
// so it is the single source of truth for the profile YAML, its ConfigMap, the
// volume/mount that projects it, and the framework env that locates it.
package serving

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
)

const (
	// ProfileVolumePrefix is the volume name that projects the profile ConfigMap.
	ProfileVolumePrefix = "aim-profile"
	// ProfileMountBase is the runtime's search root for operator-injected
	// profiles. The AIM runtime looks up profiles under
	//   /workspace/aim-runtime/profiles/custom/<aimId>/<name>
	// so mounting the assembled profile ConfigMap at <base>/<aimId> makes
	// AIM_PROFILE_ID=custom/<aimId>/<name> resolve to the file we projected.
	ProfileMountBase = "/workspace/aim-runtime/profiles/custom"
)

// ProfileYAML matches the AIM runtime's ProfileData / ModelProfileData schema
// in aim-build/src/aim_common/object_model.py.
type ProfileYAML struct {
	AimID      string            `json:"aim_id"`
	ModelID    string            `json:"model_id"`
	Metadata   ProfileMetadata   `json:"metadata"`
	EngineArgs map[string]any    `json:"engine_args"`
	EnvVars    map[string]string `json:"env_vars"`
}

// ProfileMetadata matches the AIM runtime's ProfileMetadata schema.
//
// We emit BOTH naming conventions so the assembled YAML is valid against
// every aim-runtime version we ship today:
//
//   - Legacy `gpu` / `gpu_count` are still REQUIRED by the runtime baked into
//     amdenterpriseai/aim-base:0.11 (and earlier). The runtime's Pydantic
//     ProfileMetadata schema validates `metadata.gpu` and `metadata.gpu_count`
//     as required fields; omitting them causes the predictor pod to crash
//     before opening port 8000.
//   - New `accelerator_model` / `accelerator_type` / `accelerator_count` are
//     the v1alpha2 vocabulary. Runtimes that don't know them ignore the
//     extras (Pydantic extras are not forbidden); runtimes that do can
//     consume them directly.
type ProfileMetadata struct {
	Engine              string `json:"engine"`
	GPU                 string `json:"gpu"`
	GPUCount            int32  `json:"gpu_count"`
	AcceleratorModel    string `json:"accelerator_model"`
	AcceleratorType     string `json:"accelerator_type"`
	AcceleratorCount    int32  `json:"accelerator_count"`
	ManualSelectionOnly bool   `json:"manual_selection_only"`
	Metric              string `json:"metric"`
	Precision           string `json:"precision"`
	Type                string `json:"type"`
	// Features carries optional capability tokens (e.g. "adapters") into runtime
	// ProfileMetadata; the runtime computes supports_adapters from it. Omitted
	// when empty.
	Features []string `json:"features,omitempty"`
}

// AssembleProfileYAML builds a complete profile YAML from an AIMProfileSpecCommon.
//
// The spec is expected to already reflect the effective profile configuration:
// when a consumer declared overrides, the controller has materialised an overlay
// AIMProfile and points at it, so this function does not re-merge user overrides
// on top.
func AssembleProfileYAML(spec *aimv1alpha2.AIMProfileSpecCommon) ([]byte, string, error) {
	if spec == nil {
		return nil, "", fmt.Errorf("profile spec is nil")
	}

	engineArgs := make(map[string]any)
	if spec.EngineArgs != nil && len(spec.EngineArgs.Raw) > 0 {
		if err := json.Unmarshal(spec.EngineArgs.Raw, &engineArgs); err != nil {
			return nil, "", fmt.Errorf("failed to unmarshal engineArgs: %w", err)
		}
	}

	envVars := make(map[string]string)
	if spec.EngineEnv != nil {
		for k, v := range spec.EngineEnv {
			envVars[k] = v
		}
	}

	accModel := spec.AcceleratorModel
	accType := string(spec.AcceleratorType)
	accCount := spec.AcceleratorCount

	profile := ProfileYAML{
		AimID:   spec.AimId,
		ModelID: spec.ModelId,
		Metadata: ProfileMetadata{
			Engine:              spec.Engine,
			GPU:                 accModel,
			GPUCount:            accCount,
			AcceleratorModel:    accModel,
			AcceleratorType:     accType,
			AcceleratorCount:    accCount,
			ManualSelectionOnly: false,
			Metric:              string(spec.Metric),
			Precision:           string(spec.Precision),
			Type:                string(spec.Type),
			Features:            append([]string(nil), spec.Features...),
		},
		EngineArgs: engineArgs,
		EnvVars:    envVars,
	}

	yamlBytes, err := yaml.Marshal(profile)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal profile YAML: %w", err)
	}

	filename := ProfileFilename(accModel, string(spec.Precision), accCount, string(spec.Metric))
	return yamlBytes, filename, nil
}

// ProfileFilename returns the deterministic on-disk name of the assembled
// profile YAML the runtime resolves AIM_PROFILE_ID against.
func ProfileFilename(accModel, precision string, accCount int32, metric string) string {
	accSegment := strings.ToLower(accModel)
	if accSegment == "" {
		accSegment = "none"
	}
	return fmt.Sprintf("vllm-%s-%s-tp%d-%s.yaml", accSegment, precision, accCount, metric)
}

// BuildProfileConfigMap creates a ConfigMap containing the pre-assembled profile
// YAML for mounting into a predictor pod. The caller supplies the name,
// namespace, labels, filename, and YAML so the data and the AIM_PROFILE_ID env
// var that consumes it stay consistent.
func BuildProfileConfigMap(name, namespace string, labels map[string]string, filename string, yamlBytes []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			filename: string(yamlBytes),
		},
	}
}

// BuildProfileVolume creates a Volume that projects the profile ConfigMap.
func BuildProfileVolume(configMapName string) corev1.Volume {
	return corev1.Volume{
		Name: ProfileVolumePrefix,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: configMapName,
				},
			},
		},
	}
}

// BuildProfileVolumeMount creates a VolumeMount at the AIM runtime custom profiles path.
func BuildProfileVolumeMount(aimId string) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      ProfileVolumePrefix,
		MountPath: fmt.Sprintf("%s/%s", ProfileMountBase, aimId),
		ReadOnly:  true,
	}
}

// BuildFrameworkEnvVars returns the operator-managed environment variables that
// must be present on the predictor container for the AIM runtime to locate the
// projected profile and, when model caching is active, to redirect model
// loading to the PVC-backed local path.
//
// These vars are framework-owned: neither profile.ContainerEnv nor a consumer's
// containerEnv overrides can override them. Consumers layer them on top of the
// resolved profile's containerEnv, so AIM_* identity vars cannot be reshaped
// from outside the controller.
//
// Callers pass the ProfileFilename of the profile so AIM_PROFILE_ID and the
// colocated ConfigMap's data key are derived from the SAME value: the eager bare
// CSR (BuildClusterServingRuntime) and the lazy/eager complete namespace runtime
// (BuildNamespaceServingRuntime) both feed ProfileFilename here, so the env a
// bare CSR sets can never dangle once the shadow's same-key ConfigMap lands.
func BuildFrameworkEnvVars(profileSpec *aimv1alpha2.AIMProfileSpecCommon, profileYAMLFilename string) []corev1.EnvVar {
	profileName := strings.TrimSuffix(profileYAMLFilename, ".yaml")
	aimProfileID := fmt.Sprintf("custom/%s/%s", profileSpec.AimId, profileName)

	// AIM_PROFILE_ID names the operator-assembled profile the runtime loads,
	// resolved against /workspace/aim-runtime/profiles/custom/<aimId>/<name>.yaml.
	// <name> is keyed on the profile's identity axes (via ProfileFilename), not
	// the KServe object name, so hashing the runtime/ConfigMap name never changes
	// it and it always matches the colocated ConfigMap's data key.
	//
	// A complete namespace runtime carries that ConfigMap; a bare
	// ClusterServingRuntime sets this env without it. Because AIM_PROFILE_ID always
	// names a file under the operator-owned custom/ subtree (which no image bakes),
	// the bare CSR is a shadow target in ALL cases: the file exists only once the
	// lazy shadow mounts the same-key ConfigMap — a bounded, self-healing transient
	// (see BuildClusterServingRuntime).
	vars := []corev1.EnvVar{
		{Name: constants.EnvAIMProfileID, Value: aimProfileID},
	}

	// When the profile declares modelSources, the AIMProfileCache has populated
	// a PVC that we mount at /workspace/cache/<modelId>. AIM_CACHE_PATH and
	// AIM_MODEL_ID tell the runtime to redirect --model to that local path
	// instead of pulling from HuggingFace Hub.
	//
	// The aim-runtime enforces `AIM_ID` and `AIM_MODEL_ID` as mutually exclusive
	// (one picks an image-embedded family profile, the other redirects the model
	// location). AIM_PROFILE_ID alone is sufficient to locate the custom profile,
	// so we only set AIM_ID when we are NOT redirecting the model. Per-AIM images
	// also bake `ENV AIM_ID=...` into the container so they can run standalone;
	// we explicitly clobber it to the empty string when we set AIM_MODEL_ID to
	// avoid the runtime's mutual-exclusivity check rejecting the pod.
	if len(profileSpec.ModelSources) > 0 {
		vars = append(vars,
			corev1.EnvVar{Name: constants.EnvAIMID, Value: ""},
			corev1.EnvVar{Name: constants.EnvAIMCachePath, Value: constants.AIMCacheBasePath},
			corev1.EnvVar{Name: constants.EnvAIMModelID, Value: profileSpec.ModelSources[0].ModelID},
		)
	} else {
		vars = append(vars,
			corev1.EnvVar{Name: constants.EnvAIMID, Value: profileSpec.AimId},
		)
	}

	return vars
}
