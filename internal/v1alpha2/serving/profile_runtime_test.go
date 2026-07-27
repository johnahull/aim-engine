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

	corev1 "k8s.io/api/core/v1"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
)

func sampleSpec() *aimv1alpha2.AIMProfileSpecCommon {
	return &aimv1alpha2.AIMProfileSpecCommon{
		AimId:            "qwen/qwen3-32b",
		ModelId:          "qwen/qwen3-32b-fp8",
		Engine:           "vllm",
		Metric:           aimv1alpha1.AIMMetric("latency"),
		Precision:        aimv1alpha1.AIMPrecision("fp8"),
		Type:             aimv1alpha1.AIMProfileType("optimized"),
		AcceleratorModel: "MI300X",
		AcceleratorType:  aimv1alpha1.AcceleratorType("gpu"),
		AcceleratorCount: 1,
		Image:            "ghcr.io/aim/qwen3-32b:1.0.0",
	}
}

func TestProfileFilename(t *testing.T) {
	cases := []struct {
		name      string
		accModel  string
		precision string
		accCount  int32
		metric    string
		want      string
	}{
		{"gpu profile", "MI300X", "fp8", 1, "latency", "vllm-mi300x-fp8-tp1-latency.yaml"},
		{"empty accelerator falls back to none", "", "fp16", 0, "latency", "vllm-none-fp16-tp0-latency.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProfileFilename(tc.accModel, tc.precision, tc.accCount, tc.metric); got != tc.want {
				t.Fatalf("ProfileFilename = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAssembleProfileYAML_NilSpec(t *testing.T) {
	if _, _, err := AssembleProfileYAML(nil); err == nil {
		t.Fatalf("expected error for nil spec")
	}
}

func TestBuildProfileConfigMap_CarriesNameNamespaceLabelsData(t *testing.T) {
	cm := BuildProfileConfigMap("cm", "ns", map[string]string{"k": "v"}, "vllm-mi300x-fp8-tp1-latency.yaml", []byte("aim_id: x\n"))
	if cm.Name != "cm" || cm.Namespace != "ns" {
		t.Fatalf("unexpected name/namespace: %q/%q", cm.Name, cm.Namespace)
	}
	if cm.Labels["k"] != "v" {
		t.Errorf("labels not carried: %v", cm.Labels)
	}
	if len(cm.Data) != 1 || cm.Data["vllm-mi300x-fp8-tp1-latency.yaml"] != "aim_id: x\n" {
		t.Errorf("data not carried: %v", cm.Data)
	}
}

func TestBuildProfileVolumeMount_ReadOnlyUnderBase(t *testing.T) {
	mount := BuildProfileVolumeMount("qwen/qwen3-32b")
	if !strings.HasPrefix(mount.MountPath, ProfileMountBase+"/") {
		t.Errorf("mount path %q not under %q", mount.MountPath, ProfileMountBase)
	}
	if !mount.ReadOnly {
		t.Errorf("profile mount must be read-only")
	}
	if mount.Name != ProfileVolumePrefix {
		t.Errorf("mount name = %q, want %q", mount.Name, ProfileVolumePrefix)
	}
}

// TestBuildFrameworkEnvVars_NoModelSources asserts the standalone case: only
// AIM_PROFILE_ID and AIM_ID (set to the profile's aimId) are emitted, so a
// per-AIM image can serve without a cache redirect.
func TestBuildFrameworkEnvVars_NoModelSources(t *testing.T) {
	env := envMap(BuildFrameworkEnvVars(sampleSpec(), "vllm-mi300x-fp8-tp1-latency.yaml"))

	if env[constants.EnvAIMProfileID] != "custom/qwen/qwen3-32b/vllm-mi300x-fp8-tp1-latency" {
		t.Errorf("AIM_PROFILE_ID = %q", env[constants.EnvAIMProfileID])
	}
	if env[constants.EnvAIMID] != "qwen/qwen3-32b" {
		t.Errorf("AIM_ID = %q, want the profile aimId when not redirecting", env[constants.EnvAIMID])
	}
	if _, ok := env[constants.EnvAIMModelID]; ok {
		t.Errorf("AIM_MODEL_ID must be unset without modelSources")
	}
}

// TestBuildFrameworkEnvVars_WithModelSources asserts the cache-redirect case:
// AIM_ID is clobbered empty and AIM_MODEL_ID / AIM_CACHE_PATH point the runtime
// at the locally cached weights (mutual-exclusivity with AIM_ID).
func TestBuildFrameworkEnvVars_WithModelSources(t *testing.T) {
	spec := sampleSpec()
	spec.ModelSources = []aimv1alpha1.AIMModelSource{{ModelID: "org/model", SourceURI: "hf://org/model"}}

	env := envMap(BuildFrameworkEnvVars(spec, "vllm-mi300x-fp8-tp1-latency.yaml"))

	if env[constants.EnvAIMID] != "" {
		t.Errorf("AIM_ID must be cleared when redirecting, got %q", env[constants.EnvAIMID])
	}
	if env[constants.EnvAIMModelID] != "org/model" {
		t.Errorf("AIM_MODEL_ID = %q, want org/model", env[constants.EnvAIMModelID])
	}
	if env[constants.EnvAIMCachePath] != constants.AIMCacheBasePath {
		t.Errorf("AIM_CACHE_PATH = %q, want %q", env[constants.EnvAIMCachePath], constants.AIMCacheBasePath)
	}
}

func envMap(vars []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(vars))
	for _, e := range vars {
		m[e.Name] = e.Value
	}
	return m
}
