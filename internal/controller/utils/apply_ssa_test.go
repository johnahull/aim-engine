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

package controllerutils

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests lock the contract that distinguishes the two shared apply entry
// points, so a future change cannot silently re-broaden the cooperative default
// back to the blanket ForceOwnership that PR #153 introduced and this PR reverted:
//
//   - ApplyDesiredState          → cooperative SSA (FieldOwner only). A field
//     another manager owns is left untouched; a conflicting write is rejected.
//   - ApplyDesiredStateWithForce → SSA + ForceOwnership. The manager reclaims a
//     field another manager owns and writes its value.
//
// The controller-runtime fake client (v0.22+) drives real structured-merge-diff
// field management via testing.NewFieldManagedObjectTracker, so it faithfully
// simulates SSA ownership, conflict rejection, and ForceOwnership reclaim
// without an API server. This keeps the test hermetic (no envtest binaries / no
// cluster) while still exercising true SSA semantics.
const (
	ssaTestNamespace    = "aim-ssa-test"
	ssaTestConfigMap    = "shared-config"
	ssaForeignManager   = "some-other-manager"
	ssaAimFieldManager  = "aim-test-controller"
	ssaForeignFieldData = "owned-by-foreign-manager"
	ssaAimFieldData     = "owned-by-aim-engine"
	ssaContendedKey     = "contended"
)

func ssaTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return scheme
}

// applyAsForeignManager takes SSA ownership of the contended field under a field
// manager that is NOT AIM Engine, creating the ConfigMap in the process.
func applyAsForeignManager(ctx context.Context, t *testing.T, cl client.Client) {
	t.Helper()
	foreign := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: ssaTestNamespace, Name: ssaTestConfigMap},
		Data:       map[string]string{ssaContendedKey: ssaForeignFieldData},
	}
	if err := cl.Patch(ctx, foreign, client.Apply, client.FieldOwner(ssaForeignManager)); err != nil {
		t.Fatalf("foreign manager apply failed: %v", err)
	}
}

func contendedValue(ctx context.Context, t *testing.T, cl client.Client) string {
	t.Helper()
	got := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: ssaTestNamespace, Name: ssaTestConfigMap}
	if err := cl.Get(ctx, key, got); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	return got.Data[ssaContendedKey]
}

// aimDesiredConfigMap is the object AIM Engine wants to apply: the same
// ConfigMap, but with the contended field set to AIM Engine's value.
func aimDesiredConfigMap() []client.Object {
	return []client.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ssaTestNamespace, Name: ssaTestConfigMap},
			Data:       map[string]string{ssaContendedKey: ssaAimFieldData},
		},
	}
}

// TestApplyDesiredState_CooperativeLeavesForeignField locks the cooperative
// default: it must NOT override a field another manager owns. If the default is
// ever re-broadened to ForceOwnership, the apply would succeed and overwrite the
// foreign value — this test fails in that case.
func TestApplyDesiredState_CooperativeLeavesForeignField(t *testing.T) {
	ctx := context.Background()
	scheme := ssaTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	applyAsForeignManager(ctx, t, cl)

	err := ApplyDesiredState(ctx, cl, ssaAimFieldManager, scheme, aimDesiredConfigMap(), nil)
	if err == nil {
		t.Fatal("cooperative ApplyDesiredState succeeded against a foreign-owned field; " +
			"the default must be cooperative SSA (FieldOwner only), not ForceOwnership")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("cooperative apply error = %v, want an SSA field-ownership conflict", err)
	}
	if got := contendedValue(ctx, t, cl); got != ssaForeignFieldData {
		t.Fatalf("contended field = %q after cooperative apply, want it left as the foreign value %q", got, ssaForeignFieldData)
	}
}

// TestApplyDesiredStateWithForce_ReclaimsForeignField locks the force path: it
// must reclaim a field another manager owns and write AIM Engine's value. If
// ApplyDesiredStateWithForce ever drops ForceOwnership, the apply would conflict
// and leave the foreign value — this test fails in that case.
func TestApplyDesiredStateWithForce_ReclaimsForeignField(t *testing.T) {
	ctx := context.Background()
	scheme := ssaTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	applyAsForeignManager(ctx, t, cl)

	if err := ApplyDesiredStateWithForce(ctx, cl, ssaAimFieldManager, scheme, aimDesiredConfigMap(), nil); err != nil {
		t.Fatalf("force ApplyDesiredStateWithForce failed against a foreign-owned field: %v", err)
	}
	if got := contendedValue(ctx, t, cl); got != ssaAimFieldData {
		t.Fatalf("contended field = %q after force apply, want it reclaimed to AIM Engine's value %q", got, ssaAimFieldData)
	}
}

// TestApplyDesiredState_ReassertsOwnField documents why cooperative SSA is the
// correct default for controllers that exclusively own their children (i.e.
// every pipeline controller that did NOT rename its field manager): cooperative
// apply still updates fields AIM Engine itself owns, so drift on owned fields is
// reconciled without force. Force is only needed to reclaim a field another
// manager owns (a field-manager rename, or the reserved-prefix projected
// runtimes handled via ApplyWithForce).
func TestApplyDesiredState_ReassertsOwnField(t *testing.T) {
	ctx := context.Background()
	scheme := ssaTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()

	// AIM Engine first establishes ownership of the field.
	first := []client.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: ssaTestNamespace, Name: ssaTestConfigMap},
			Data:       map[string]string{ssaContendedKey: ssaForeignFieldData},
		},
	}
	if err := ApplyDesiredState(ctx, cl, ssaAimFieldManager, scheme, first, nil); err != nil {
		t.Fatalf("initial cooperative apply failed: %v", err)
	}

	// A second cooperative apply changing the same field must succeed and take
	// effect, because AIM Engine owns it — no force required.
	if err := ApplyDesiredState(ctx, cl, ssaAimFieldManager, scheme, aimDesiredConfigMap(), nil); err != nil {
		t.Fatalf("cooperative re-apply of an AIM-owned field failed: %v", err)
	}
	if got := contendedValue(ctx, t, cl); got != ssaAimFieldData {
		t.Fatalf("contended field = %q after cooperative re-apply, want %q", got, ssaAimFieldData)
	}
}
