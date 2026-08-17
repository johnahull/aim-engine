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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	aimv1alpha2 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha2"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
)

func primaryCPUSpec(profileType aimv1alpha1.AIMProfileType) aimv1alpha2.AIMProfileSpecCommon {
	return aimv1alpha2.AIMProfileSpecCommon{
		AimId:   "qwen/qwen3-32b",
		Image:   "registry.io/qwen3-32b:1.0.0",
		Primary: true,
		Type:    profileType,
		ModelSources: []aimv1alpha1.AIMModelSource{{
			ModelID:   "qwen/qwen3-32b",
			SourceURI: "hf://qwen/qwen3-32b",
		}},
	}
}

func TestNamespaceModelSlugElectionProjectsOnlyRankedPrimary(t *testing.T) {
	// The alphabetically earlier profile intentionally has the lower rank. This
	// proves the winner comes from the shared profile ranker rather than list or
	// reconcile order.
	lower := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "a-general", Namespace: "team-a"},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: primaryCPUSpec(aimv1alpha1.AIMProfileTypeGeneral),
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
	}
	winner := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "z-optimized", Namespace: "team-a"},
		Spec: aimv1alpha2.AIMProfileSpec{
			AIMProfileSpecCommon: primaryCPUSpec(aimv1alpha1.AIMProfileTypeOptimized),
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
	}
	c := newProfileTestClient(t, lower, winner)

	for _, mode := range []aimv1alpha2.RuntimeProjectionMode{
		aimv1alpha2.RuntimeProjectionModeReduced,
		aimv1alpha2.RuntimeProjectionModeBoth,
	} {
		t.Run(string(mode), func(t *testing.T) {
			r := &ProfileReconciler{Client: c, ProjectionMode: mode}

			lowerCtx := controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: lower}
			lowerObs := r.ComposeState(context.Background(), lowerCtx, r.FetchRemoteState(context.Background(), c, lowerCtx))
			if lowerObs.modelSlugWinner {
				t.Fatalf("lower-ranked primary %q won the model-slug election", lower.Name)
			}
			lowerPlan := r.PlanResources(context.Background(), lowerCtx, lowerObs)

			winnerCtx := controllerutils.ReconcileContext[*aimv1alpha2.AIMProfile]{Object: winner}
			winnerObs := r.ComposeState(context.Background(), winnerCtx, r.FetchRemoteState(context.Background(), c, winnerCtx))
			if !winnerObs.modelSlugWinner {
				t.Fatalf("higher-ranked primary %q did not win the model-slug election", winner.Name)
			}
			winnerPlan := r.PlanResources(context.Background(), winnerCtx, winnerObs)

			wantLower, wantWinner := 0, 1
			if mode == aimv1alpha2.RuntimeProjectionModeBoth {
				wantLower, wantWinner = 1, 2
			}
			if got := countServingRuntimes(lowerPlan.GetToApplyWithForce()); got != wantLower {
				t.Fatalf("lower-ranked profile projected %d runtimes, want %d", got, wantLower)
			}
			if got := countServingRuntimes(winnerPlan.GetToApplyWithForce()); got != wantWinner {
				t.Fatalf("winner projected %d runtimes, want %d", got, wantWinner)
			}
		})
	}
}

func TestClusterModelSlugElectionProjectsOnlyRankedPrimary(t *testing.T) {
	lower := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "a-general"},
		Spec: aimv1alpha2.AIMClusterProfileSpec{
			AIMProfileSpecCommon: primaryCPUSpec(aimv1alpha1.AIMProfileTypeGeneral),
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
	}
	winner := &aimv1alpha2.AIMClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "z-optimized"},
		Spec: aimv1alpha2.AIMClusterProfileSpec{
			AIMProfileSpecCommon: primaryCPUSpec(aimv1alpha1.AIMProfileTypeOptimized),
		},
		Status: aimv1alpha2.AIMProfileStatus{Status: constants.AIMStatusReady},
	}
	c := newProfileTestClient(t, lower, winner)
	r := &ClusterProfileReconciler{Client: c, ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}

	lowerCtx := controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: lower}
	lowerObs := r.ComposeState(context.Background(), lowerCtx, r.FetchRemoteState(context.Background(), c, lowerCtx))
	if lowerObs.modelSlugWinner {
		t.Fatalf("lower-ranked cluster primary %q won the model-slug election", lower.Name)
	}
	lowerPlan := r.PlanResources(context.Background(), lowerCtx, lowerObs)
	if got := countClusterServingRuntimes(lowerPlan.GetToApplyWithForce()); got != 0 {
		t.Fatalf("lower-ranked cluster primary projected %d model-slug runtimes, want 0", got)
	}

	winnerCtx := controllerutils.ReconcileContext[*aimv1alpha2.AIMClusterProfile]{Object: winner}
	winnerObs := r.ComposeState(context.Background(), winnerCtx, r.FetchRemoteState(context.Background(), c, winnerCtx))
	if !winnerObs.modelSlugWinner {
		t.Fatalf("higher-ranked cluster primary %q did not win the model-slug election", winner.Name)
	}
	winnerPlan := r.PlanResources(context.Background(), winnerCtx, winnerObs)
	if got := countClusterServingRuntimes(winnerPlan.GetToApplyWithForce()); got != 1 {
		t.Fatalf("cluster winner projected %d model-slug runtimes, want 1", got)
	}
}

func TestLosingModelSlugElectionClearsStatusClaim(t *testing.T) {
	spec := primaryCPUSpec(aimv1alpha1.AIMProfileTypeGeneral)
	profile := &aimv1alpha2.AIMProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "loser", Namespace: "team-a"},
		Spec:       aimv1alpha2.AIMProfileSpec{AIMProfileSpecCommon: spec},
	}
	obs := ProfileObservation{
		ProfileFetchResult: ProfileFetchResult{profile: profile, modelSlugWinner: false},
		deployable:         true,
		projectable:        true,
	}
	status := &aimv1alpha2.AIMProfileStatus{
		ProjectedModelSlugRuntimeName: "aim-qwen-qwen3-32b",
	}

	r := &ProfileReconciler{ProjectionMode: aimv1alpha2.RuntimeProjectionModeReduced}
	r.DecorateStatus(status, controllerutils.NewConditionManager(nil), obs)

	if status.ProjectedModelSlugRuntimeName != "" {
		t.Fatalf("losing profile kept stale model-slug status claim %q", status.ProjectedModelSlugRuntimeName)
	}
}
