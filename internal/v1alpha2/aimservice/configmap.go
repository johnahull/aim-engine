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
	corev1 "k8s.io/api/core/v1"

	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/utils"
	"github.com/amd-enterprise-ai/aim-engine/internal/v1alpha2/serving"
)

// The profile→serving building blocks (profile YAML, ConfigMap, volume/mount,
// framework env) live in internal/v1alpha2/serving so the profile reconcilers
// and AIMService share one source of truth. The names below are thin,
// service-flavoured adapters over that package.

const (
	profileVolumePrefix = serving.ProfileVolumePrefix
	profileMountBase    = serving.ProfileMountBase
)

// profileYAML is the runtime's profile schema; aliased so package-local code and
// tests can keep referencing it unqualified.
type profileYAML = serving.ProfileYAML

// assembleProfileYAML builds a complete profile YAML from the resolved spec.
func assembleProfileYAML(spec *aimv1alpha2.AIMProfileSpecCommon) ([]byte, string, error) {
	return serving.AssembleProfileYAML(spec)
}

func profileFilename(accModel, precision string, accCount int32, metric string) string {
	return serving.ProfileFilename(accModel, precision, accCount, metric)
}

// BuildProfileVolume creates a Volume that projects the profile ConfigMap.
func BuildProfileVolume(configMapName string) corev1.Volume {
	return serving.BuildProfileVolume(configMapName)
}

// BuildProfileVolumeMount creates a VolumeMount at the AIM runtime custom profiles path.
func BuildProfileVolumeMount(aimId string) corev1.VolumeMount {
	return serving.BuildProfileVolumeMount(aimId)
}

// legacyProfileConfigMapName reconstructs the deterministic name of the
// service-owned profile ConfigMap that pre-ADR-0008 AIMService reconciles
// created (<service>-profile-<hash>) through the now-removed profileConfigMapName
// helper. The runtime-reference rewrite stopped planning that ConfigMap, so a
// service already running when the operator is upgraded leaves it orphaned. The
// reconcile Plan step deletes it once (see planLegacyProfileConfigMapCleanup),
// guarded by owner-ref + managed-by label so only AIM Engine's own orphan is
// touched. The derivation must match the retired code EXACTLY so the delete
// targets the orphan and nothing else — recovered verbatim from git history
// (commit 528af2c, internal/v1alpha2/aimservice/configmap.go).
func legacyProfileConfigMapName(serviceName string) (string, error) {
	return utils.GenerateDerivedName(
		[]string{serviceName, "profile"},
		utils.WithHashSource(serviceName, "service-profile"),
	)
}
