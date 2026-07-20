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

package aimartifact

import (
	"testing"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
	controllerutils "github.com/amd-enterprise-ai/aim-engine/internal/controller/utils"
)

func TestFoldStalledFilesystemHealth(t *testing.T) {
	const warning = "Filesystem may be stuck: 'du' exceeded 300s. Progress and stall detection paused."

	running := controllerutils.ComponentHealth{
		Component: "DownloadJob",
		State:     constants.AIMStatusProgressing,
		Reason:    "Running",
		Message:   "Job is running",
	}

	tests := []struct {
		name        string
		jobHealth   controllerutils.ComponentHealth
		progress    *aimv1alpha1.DownloadProgress
		wantState   constants.AIMStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:        "no progress → unchanged",
			jobHealth:   running,
			progress:    nil,
			wantState:   constants.AIMStatusProgressing,
			wantReason:  "Running",
			wantMessage: "Job is running",
		},
		{
			name:        "empty message → unchanged",
			jobHealth:   running,
			progress:    &aimv1alpha1.DownloadProgress{Message: ""},
			wantState:   constants.AIMStatusProgressing,
			wantReason:  "Running",
			wantMessage: "Job is running",
		},
		{
			name:        "running job with warning → Degraded carrying the warning",
			jobHealth:   running,
			progress:    &aimv1alpha1.DownloadProgress{Message: warning},
			wantState:   constants.AIMStatusDegraded,
			wantReason:  aimv1alpha1.ArtifactReasonFilesystemStalled,
			wantMessage: warning,
		},
		{
			name:        "succeeded job → warning ignored (success wins)",
			jobHealth:   controllerutils.ComponentHealth{Component: "DownloadJob", State: constants.AIMStatusReady, Reason: "Complete", Message: "done"},
			progress:    &aimv1alpha1.DownloadProgress{Message: warning},
			wantState:   constants.AIMStatusReady,
			wantReason:  "Complete",
			wantMessage: "done",
		},
		{
			name:        "failed job → warning ignored (failure wins)",
			jobHealth:   controllerutils.ComponentHealth{Component: "DownloadJob", State: constants.AIMStatusFailed, Reason: "BackoffLimitExceeded", Message: "job failed"},
			progress:    &aimv1alpha1.DownloadProgress{Message: warning},
			wantState:   constants.AIMStatusFailed,
			wantReason:  "BackoffLimitExceeded",
			wantMessage: "job failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := foldStalledFilesystemHealth(tt.jobHealth, tt.progress)
			if got.GetState() != tt.wantState {
				t.Errorf("state = %q, want %q", got.GetState(), tt.wantState)
			}
			if got.GetReason() != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.GetReason(), tt.wantReason)
			}
			if got.GetMessage() != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.GetMessage(), tt.wantMessage)
			}
		})
	}
}
