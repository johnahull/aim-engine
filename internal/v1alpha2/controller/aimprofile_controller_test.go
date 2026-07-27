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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
)

// TestFindProfilesForProfileCache pins the eager cache watch's fan-out: a changed
// namespace-scope AIMProfileCache enqueues the namespace AIMProfile it caches (so
// a late-Ready service-driven cache re-projects the runtime that mounts it),
// while a cluster-scope cache, an empty profileName, and a non-cache object all
// enqueue nothing.
func TestFindProfilesForProfileCache(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		obj  client.Object
		want []string
	}{
		{
			name: "namespace-scope cache enqueues its profile",
			obj:  makeProfileCache("myprofile-cache-fe2d5ebc", testNamespace, "myprofile", aimv1alpha1.AIMResolutionScopeNamespace),
			want: []string{testNamespace + "/myprofile"},
		},
		{
			// The enqueued profile lives in the cache's OWN namespace, so a cache
			// in a different namespace maps to that namespace's profile.
			name: "cache maps to the profile in its own namespace",
			obj:  makeProfileCache("myprofile-cache", "team-b", "myprofile", aimv1alpha1.AIMResolutionScopeNamespace),
			want: []string{"team-b/myprofile"},
		},
		{
			name: "empty scope is treated as namespace",
			obj:  makeProfileCache("myprofile-cache", testNamespace, "myprofile", ""),
			want: []string{testNamespace + "/myprofile"},
		},
		{
			name: "cluster-scope cache enqueues nothing (bare CSR mounts no cache)",
			obj:  makeProfileCache("myprofile-cache", testNamespace, "myprofile", aimv1alpha1.AIMResolutionScopeCluster),
			want: nil,
		},
		{
			name: "empty profileName enqueues nothing",
			obj:  makeProfileCache("orphan-cache", testNamespace, "", aimv1alpha1.AIMResolutionScopeNamespace),
			want: nil,
		},
		{
			name: "non-cache object enqueues nothing",
			obj:  &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: testNamespace}},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := findProfilesForProfileCache(context.Background(), tc.obj)
			assertRequestKeys(t, got, tc.want)
		})
	}
}
