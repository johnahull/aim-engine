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

package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
)

const testRuntimeConfigName = "test"

func TestAIMRuntimeConfigReconcilerWarnsForDeprecatedArtifactCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := aimv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	config := &aimv1alpha1.AIMRuntimeConfig{}
	config.Name = testRuntimeConfigName
	config.Namespace = "models"
	config.Spec.ArtifactCache = deprecatedArtifactCacheForTest()

	recorder := record.NewFakeRecorder(1)
	reconciler := &AIMRuntimeConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build(),
		Recorder: recorder,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: config.Name, Namespace: config.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertArtifactCacheDeprecatedEvent(t, recorder)
}

func TestAIMClusterRuntimeConfigReconcilerWarnsForDeprecatedArtifactCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := aimv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	config := &aimv1alpha1.AIMClusterRuntimeConfig{}
	config.Name = testRuntimeConfigName
	config.Spec.ArtifactCache = deprecatedArtifactCacheForTest()

	recorder := record.NewFakeRecorder(1)
	reconciler := &AIMClusterRuntimeConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build(),
		Recorder: recorder,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: config.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertArtifactCacheDeprecatedEvent(t, recorder)
}

func TestAIMRuntimeConfigReconcilerDoesNotWarnWithoutArtifactCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := aimv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	config := &aimv1alpha1.AIMRuntimeConfig{}
	config.Name = testRuntimeConfigName
	config.Namespace = "models"

	recorder := record.NewFakeRecorder(1)
	reconciler := &AIMRuntimeConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build(),
		Recorder: recorder,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: config.Name, Namespace: config.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event: %s", event)
	default:
	}
}

//nolint:staticcheck // This test intentionally exercises deprecated compatibility behavior.
func deprecatedArtifactCacheForTest() *aimv1alpha1.ArtifactCacheConfig {
	return &aimv1alpha1.ArtifactCacheConfig{Enabled: true}
}

func assertArtifactCacheDeprecatedEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning "+artifactCacheDeprecatedReason) {
			t.Fatalf("unexpected event: %s", event)
		}
		if !strings.Contains(event, "no longer has any effect") {
			t.Fatalf("event does not explain removed behavior: %s", event)
		}
	default:
		t.Fatal("expected deprecation warning event")
	}
}
