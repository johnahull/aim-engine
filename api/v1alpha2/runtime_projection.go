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

package v1alpha2

import "fmt"

// RuntimeProjectionMode governs the eager runtime projection the profile
// reconcilers perform (AIMClusterProfile → ClusterServingRuntime, AIMProfile →
// ServingRuntime). It is runtime-neutral: the same value governs both the
// cluster and namespace projections. The lazy InferenceService-watch projection
// is always on and is NOT governed by this knob.
//
// +kubebuilder:validation:Enum=Exhaustive;Reduced;Both
type RuntimeProjectionMode string

const (
	// RuntimeProjectionModeExhaustive projects one runtime per projectable
	// profile, named aim-<profile.Name>, with autoSelect off so native KServe
	// auto-selection is never made ambiguous by N runtimes claiming the same
	// model format.
	RuntimeProjectionModeExhaustive RuntimeProjectionMode = "Exhaustive"

	// RuntimeProjectionModeReduced publishes one model-slug primary runtime per
	// model (named aim-<model-slug>) that native KServe references by name.
	// autoSelect stays off like every projected runtime, since all share one
	// model format and would otherwise collide across models.
	RuntimeProjectionModeReduced RuntimeProjectionMode = "Reduced"

	// RuntimeProjectionModeBoth runs Exhaustive and Reduced projection together:
	// per-profile runtimes plus the model-slug primary.
	RuntimeProjectionModeBoth RuntimeProjectionMode = "Both"

	// RuntimeProjectionModeDefault is the projection mode used when none is
	// configured.
	RuntimeProjectionModeDefault = RuntimeProjectionModeBoth
)

// ParseRuntimeProjectionMode validates a configured projection mode string,
// defaulting an empty value to the default mode and rejecting unknown values
// explicitly (least surprise: a typo fails loudly at startup rather than
// silently disabling projection).
func ParseRuntimeProjectionMode(value string) (RuntimeProjectionMode, error) {
	switch RuntimeProjectionMode(value) {
	case "":
		return RuntimeProjectionModeDefault, nil
	case RuntimeProjectionModeExhaustive, RuntimeProjectionModeReduced, RuntimeProjectionModeBoth:
		return RuntimeProjectionMode(value), nil
	default:
		return "", fmt.Errorf("invalid runtime projection mode %q (want one of Exhaustive, Reduced, Both)", value)
	}
}

// ProjectsPerProfile reports whether the mode projects a runtime per profile
// (the Exhaustive behavior). True for Exhaustive and Both.
func (m RuntimeProjectionMode) ProjectsPerProfile() bool {
	return m == RuntimeProjectionModeExhaustive || m == RuntimeProjectionModeBoth
}

// ProjectsModelSlug reports whether the mode publishes a model-slug primary
// runtime per model (the Reduced behavior). True for Reduced and Both. The
// eager profile reconcilers gate the model-slug projection on this predicate and
// the lazy InferenceService-watch completion keys off the same Reduced/Both
// behavior.
func (m RuntimeProjectionMode) ProjectsModelSlug() bool {
	return m == RuntimeProjectionModeReduced || m == RuntimeProjectionModeBoth
}
