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
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
)

func directVLLMSpec() *aimv1alpha2.AIMProfileSpecCommon {
	return &aimv1alpha2.AIMProfileSpecCommon{
		AimId:             "org/model",
		ModelId:           "org/model",
		Engine:            "vllm",
		AcceleratorVendor: aimv1alpha1.AcceleratorVendorNVIDIA,
		AcceleratorType:   aimv1alpha1.AcceleratorTypeGPU,
		AcceleratorCount:  2,
		ModelSources: []aimv1alpha1.AIMModelSource{
			{ModelID: "org/model", SourceURI: "hf://org/model"},
		},
	}
}

func TestBuildEngineInvocation_DirectVLLMDefaults(t *testing.T) {
	invocation, err := buildEngineInvocation(directVLLMSpec())
	if err != nil {
		t.Fatalf("buildEngineInvocation: %v", err)
	}
	if !slices.Equal(invocation.Command, []string{"vllm", "serve"}) {
		t.Fatalf("command = %#v", invocation.Command)
	}
	want := []string{
		"$(AIM_VLLM_MODEL)",
		"--served-model-name=org/model",
		"--host=0.0.0.0",
		"--port=8000",
		"--tensor-parallel-size=2",
	}
	if !slices.Equal(invocation.Args, want) {
		t.Fatalf("args = %#v, want %#v", invocation.Args, want)
	}
}

func TestBuildEngineInvocation_EngineArgsOverrideGeneratedTuning(t *testing.T) {
	spec := directVLLMSpec()
	spec.EngineArgs = &apiextensionsv1.JSON{Raw: []byte(`{
		"served_model_name": "alias",
		"tensor-parallel-size": 1,
		"gpu-memory-utilization": 0.9,
		"disable-log-stats": null,
		"enforce-eager": true,
		"enable-prefix-caching": false
	}`)}

	invocation, err := buildEngineInvocation(spec)
	if err != nil {
		t.Fatalf("buildEngineInvocation: %v", err)
	}
	want := []string{
		"$(AIM_VLLM_MODEL)",
		"--host=0.0.0.0",
		"--port=8000",
		"--disable-log-stats",
		"--no-enable-prefix-caching",
		"--enforce-eager",
		"--gpu-memory-utilization=0.9",
		"--served-model-name=alias",
		"--tensor-parallel-size=1",
	}
	if !slices.Equal(invocation.Args, want) {
		t.Fatalf("args = %#v, want %#v", invocation.Args, want)
	}
}

func TestBuildEngineInvocation_RejectsFrameworkManagedArgs(t *testing.T) {
	spec := directVLLMSpec()
	spec.EngineArgs = &apiextensionsv1.JSON{Raw: []byte(`{"port": 9000}`)}

	if _, err := buildEngineInvocation(spec); err == nil {
		t.Fatal("expected framework-managed port override to fail")
	}
}

func TestBuildRuntimeEnv_DirectVLLMWithoutCacheUsesOnlineModelID(t *testing.T) {
	spec := directVLLMSpec()
	spec.EngineEnv = map[string]string{"VLLM_DO_NOT_TRACK": "1"}

	reference := resolveVLLMModelReference(spec, nil)
	env := buildRuntimeEnv(spec, "unused.yaml", reference)
	values := envMap(env)
	if values["VLLM_DO_NOT_TRACK"] != "1" {
		t.Fatalf("VLLM_DO_NOT_TRACK = %q", values["VLLM_DO_NOT_TRACK"])
	}
	if values[constants.EnvAIMVLLMModel] != "org/model" {
		t.Fatalf("%s = %q, want online model ID", constants.EnvAIMVLLMModel, values[constants.EnvAIMVLLMModel])
	}
	if values["HF_HUB_OFFLINE"] != "0" || values["TRANSFORMERS_OFFLINE"] != "0" {
		t.Fatalf("cacheless direct vLLM must remain online: %#v", values)
	}
	if _, exists := values["AIM_PROFILE_ID"]; exists {
		t.Fatalf("direct vLLM must not receive AIM runtime identity env: %#v", values)
	}
}

func TestResolveVLLMModelReference_UsesReadyArtifactMountPoint(t *testing.T) {
	cache := &aimv1alpha2.AIMProfileCache{
		Status: aimv1alpha2.AIMProfileCacheStatus{
			Status: constants.AIMStatusReady,
			Artifacts: map[string]aimv1alpha1.AIMResolvedArtifact{
				"model": {
					Name:                  "artifact",
					Model:                 "org/model",
					Status:                constants.AIMStatusReady,
					PersistentVolumeClaim: "model-pvc",
					MountPoint:            "/models/cached",
				},
			},
		},
	}
	got := resolveVLLMModelReference(directVLLMSpec(), cache)
	if got.value != "/models/cached" || !got.local {
		t.Fatalf("resolveVLLMModelReference = %+v, want local /models/cached", got)
	}
	values := envMap(buildRuntimeEnv(directVLLMSpec(), "unused.yaml", got))
	if values["HF_HUB_OFFLINE"] != "1" || values["TRANSFORMERS_OFFLINE"] != "1" {
		t.Fatalf("ready local cache must force offline mode: %#v", values)
	}
}
