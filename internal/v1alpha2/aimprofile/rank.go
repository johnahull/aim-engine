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
	"sort"

	"github.com/blang/semver/v4"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	"github.com/amd-enterprise-ai/aim-engine/internal/utils"
)

// SortNamespaceProfiles orders profiles so SelectBestNamespaceProfile produces
// the shared deterministic profile ranking: status first, followed by primary,
// type, accelerator model, metric, precision, accelerator count, accelerator
// type, version, and finally alphabetical name.
func SortNamespaceProfiles(profiles []aimv1alpha2.AIMProfile) {
	sort.SliceStable(profiles, func(i, j int) bool {
		return ProfileLess(
			&profiles[i].Spec.AIMProfileSpecCommon,
			&profiles[j].Spec.AIMProfileSpecCommon,
			profiles[i].Status.Version,
			profiles[j].Status.Version,
			profiles[i].Name,
			profiles[j].Name,
		)
	})
}

// SortClusterProfiles is the cluster-scoped counterpart of
// SortNamespaceProfiles.
func SortClusterProfiles(profiles []aimv1alpha2.AIMClusterProfile) {
	sort.SliceStable(profiles, func(i, j int) bool {
		return ProfileLess(
			&profiles[i].Spec.AIMProfileSpecCommon,
			&profiles[j].Spec.AIMProfileSpecCommon,
			profiles[i].Status.Version,
			profiles[j].Status.Version,
			profiles[i].Name,
			profiles[j].Name,
		)
	})
}

// SelectBestNamespaceProfile selects the highest-ranked namespace profile.
// SortNamespaceProfiles must be called first so equal-status candidates retain
// the deterministic spec/name ordering.
func SelectBestNamespaceProfile(profiles []aimv1alpha2.AIMProfile) *aimv1alpha2.AIMProfile {
	return utils.SelectBestPtr(profiles, func(p *aimv1alpha2.AIMProfile) constants.AIMStatus {
		return p.Status.GetAIMStatus()
	})
}

// SelectBestClusterProfile is the cluster-scoped counterpart of
// SelectBestNamespaceProfile.
func SelectBestClusterProfile(profiles []aimv1alpha2.AIMClusterProfile) *aimv1alpha2.AIMClusterProfile {
	return utils.SelectBestPtr(profiles, func(p *aimv1alpha2.AIMClusterProfile) constants.AIMStatus {
		return p.Status.GetAIMStatus()
	})
}

// ProfileLess reports whether a ranks ahead of b across the shared profile
// ranking, excluding status (which SelectBest* applies first).
//
// Tier order:
//  1. Primary
//  2. Type
//  3. AcceleratorModel
//  4. Metric
//  5. Precision
//  6. AcceleratorCount
//  7. AcceleratorType
//  8. Version
//  9. Name
func ProfileLess(a, b *aimv1alpha2.AIMProfileSpecCommon, aVer, bVer, aName, bName string) bool {
	if cmp := CompareProfileRankIgnoringName(a, b, aVer, bVer); cmp != 0 {
		return cmp < 0
	}
	return aName < bName
}

// CompareProfileRankIgnoringName returns -1 / 0 / +1 reflecting whether a
// ranks ahead of, equal to, or behind b across every rank tier except the
// alphabetical-name last resort.
func CompareProfileRankIgnoringName(a, b *aimv1alpha2.AIMProfileSpecCommon, aVer, bVer string) int {
	if a.Primary != b.Primary {
		if a.Primary {
			return -1
		}
		return 1
	}
	if rank := ProfileTypeRank(a.Type) - ProfileTypeRank(b.Type); rank != 0 {
		return signOf(rank)
	}
	if rank := acceleratorModelRank(a.AcceleratorModel) - acceleratorModelRank(b.AcceleratorModel); rank != 0 {
		return signOf(rank)
	}
	if rank := metricRank(a.Metric) - metricRank(b.Metric); rank != 0 {
		return signOf(rank)
	}
	if rank := precisionRank(a.Precision) - precisionRank(b.Precision); rank != 0 {
		return signOf(rank)
	}
	if a.AcceleratorCount != b.AcceleratorCount {
		switch {
		case a.AcceleratorCount == 0:
			return 1
		case b.AcceleratorCount == 0:
			return -1
		case a.AcceleratorCount < b.AcceleratorCount:
			return -1
		default:
			return 1
		}
	}
	if a.AcceleratorType != b.AcceleratorType {
		switch {
		case a.AcceleratorType == aimv1alpha1.AcceleratorTypeGPU:
			return -1
		case b.AcceleratorType == aimv1alpha1.AcceleratorTypeGPU:
			return 1
		}
	}
	if cmp := compareProfileVersions(aVer, bVer); cmp != 0 {
		return -cmp
	}
	return 0
}

func signOf(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func acceleratorModelRank(model string) int {
	return utils.PreferenceScore(model, acceleratorModelPrefMap)
}

func metricRank(metric aimv1alpha1.AIMMetric) int {
	return utils.PreferenceScore(string(metric), metricPrefMap)
}

func precisionRank(precision aimv1alpha1.AIMPrecision) int {
	return utils.PreferenceScore(string(precision), precisionPrefMap)
}

var (
	acceleratorModelPrefMap = utils.MakePreferenceMap(utils.AcceleratorModelPreferenceOrder)
	metricPrefMap           = utils.MakePreferenceMap(utils.MetricPreferenceOrder)
	precisionPrefMap        = utils.MakePreferenceMap(utils.PrecisionPreferenceOrder)
)

func compareProfileVersions(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return -1
	case b == "":
		return 1
	}

	aSemver, aErr := semver.ParseTolerant(a)
	bSemver, bErr := semver.ParseTolerant(b)
	switch {
	case aErr == nil && bErr == nil:
		return aSemver.Compare(bSemver)
	case aErr == nil:
		return 1
	case bErr == nil:
		return -1
	default:
		return compareVersionStrings(a, b)
	}
}
