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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aimv1alpha1 "github.com/amd-enterprise-ai/aim-engine/api/v1alpha1"
	"github.com/amd-enterprise-ai/aim-engine/internal/constants"
)

const templateCacheTestNamespace = "aim-template-cache-test"

// newTemplateCacheTestReconciler mirrors the production wiring, including the
// status.artifacts index the artifact watch resolves through. Without WithIndex a
// MatchingFields List errors instead of matching, so the index path has to be
// registered here for the tests below to mean anything.
func newTemplateCacheTestReconciler(t *testing.T, objs ...client.Object) *AIMTemplateCacheReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := aimv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	return &AIMTemplateCacheReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objs...).
			WithIndex(&aimv1alpha1.AIMTemplateCache{}, aimv1alpha1.TemplateCacheArtifactNameIndexKey, indexTemplateCacheArtifactNames).
			Build(),
		Scheme: scheme,
	}
}

func newTestTemplateCache(name string, mode aimv1alpha1.AIMTemplateCacheMode) *aimv1alpha1.AIMTemplateCache {
	return &aimv1alpha1.AIMTemplateCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: templateCacheTestNamespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: aimv1alpha1.AIMTemplateCacheSpec{
			TemplateName:  name + "-template",
			TemplateScope: aimv1alpha1.AIMServiceTemplateScopeNamespace,
			Mode:          mode,
		},
	}
}

func newTestArtifact(name string) *aimv1alpha1.AIMArtifact {
	return &aimv1alpha1.AIMArtifact{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: templateCacheTestNamespace,
		},
		Spec: aimv1alpha1.AIMArtifactSpec{
			SourceURI: "hf://meta-llama/Llama-3.1-8B-Instruct",
		},
	}
}

// resolvingArtifact records artifactName in the cache's status the way a completed
// reconcile does. This is what the artifact-name index is built from, so it is also
// what makes a cache an exact match for an artifact event.
func resolvingArtifact(tc *aimv1alpha1.AIMTemplateCache, artifactName string) *aimv1alpha1.AIMTemplateCache {
	if tc.Status.Artifacts == nil {
		tc.Status.Artifacts = map[string]aimv1alpha1.AIMResolvedArtifact{}
	}
	tc.Status.Artifacts[artifactName] = aimv1alpha1.AIMResolvedArtifact{
		UID:    artifactName + "-uid",
		Name:   artifactName,
		Model:  tc.Spec.TemplateName + "-model",
		Status: constants.AIMStatusReady,
	}
	return tc
}

func ownedByTemplateCache(tc *aimv1alpha1.AIMTemplateCache) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: aimv1alpha1.GroupVersion.String(),
		Kind:       "AIMTemplateCache",
		Name:       tc.Name,
		UID:        tc.UID,
	}
}

func mappedTemplateCaches(t *testing.T, r *AIMTemplateCacheReconciler, artifact *aimv1alpha1.AIMArtifact) []string {
	t.Helper()
	got := requestNames(r.findTemplateCachesForArtifact(context.Background(), artifact))
	slices.Sort(got)
	return got
}

// The candidate scan cannot compare source URIs without resolving each cache's template,
// so it may over-select eligible caches. Indexed resolvers also appear in that scan and
// must be deduplicated in the returned union.
func TestFindTemplateCachesForArtifact_DeduplicatesIndexedAndEligibleCaches(t *testing.T) {
	artifact := newTestArtifact("hf---meta-llama-llama-3-1-8b-instruct-d4dc6f81cc")

	creator := resolvingArtifact(newTestTemplateCache("cache-mi300x-thr", aimv1alpha1.TemplateCacheModeShared), artifact.Name)
	adopter := resolvingArtifact(newTestTemplateCache("cache-mi300x-lat", aimv1alpha1.TemplateCacheModeShared), artifact.Name)
	// Same namespace, same mode, no storage class - so ArtifactAdoptableBy says yes -
	// but it tracks a different model source entirely.
	bystander := resolvingArtifact(newTestTemplateCache("cache-mistral", aimv1alpha1.TemplateCacheModeShared), "hf---mistralai-mistral-7b-fa03e1b7c2")

	r := newTemplateCacheTestReconciler(t, creator, adopter, bystander, artifact)

	got := mappedTemplateCaches(t, r, artifact)
	want := []string{adopter.Name, bystander.Name, creator.Name}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// A brand-new artifact is in nobody's status yet, so eligible namespace candidates are
// the only requests available. Without them first resolution strands.
func TestFindTemplateCachesForArtifact_IncludesEligibleCachesWithoutIndex(t *testing.T) {
	fresh := newTestArtifact("hf---meta-llama-llama-3-1-8b-instruct-d4dc6f81cc")

	waiting := newTestTemplateCache("cache-waiting", aimv1alpha1.TemplateCacheModeShared)
	// Populates the index with a different key, so an empty result for the fresh
	// artifact is a genuine miss rather than an unregistered index.
	elsewhere := resolvingArtifact(newTestTemplateCache("cache-elsewhere", aimv1alpha1.TemplateCacheModeShared), "hf---mistralai-mistral-7b-fa03e1b7c2")

	r := newTemplateCacheTestReconciler(t, waiting, elsewhere, fresh)

	got := mappedTemplateCaches(t, r, fresh)
	want := []string{elsewhere.Name, waiting.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// A nonempty index can still be incomplete while another adopter has not published the
// artifact in status. The indexed resolver and eligible candidate must both be woken.
func TestFindTemplateCachesForArtifact_IncludesAdopterThatHasNotPublishedYet(t *testing.T) {
	artifact := newTestArtifact("hf---meta-llama-llama-3-1-8b-instruct-d4dc6f81cc")

	published := resolvingArtifact(newTestTemplateCache("cache-published", aimv1alpha1.TemplateCacheModeShared), artifact.Name)
	// Resolves the same source and would be returned by the candidate scan, but has not
	// completed the reconcile that would publish the artifact into its status.
	unpublished := newTestTemplateCache("cache-unpublished", aimv1alpha1.TemplateCacheModeShared)

	r := newTemplateCacheTestReconciler(t, published, unpublished, artifact)

	got := mappedTemplateCaches(t, r, artifact)
	want := []string{published.Name, unpublished.Name}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// Most caches waiting out a terminating artifact have dropped it from status.artifacts,
// but one lagging resolver can keep the exact index nonempty for the final Delete event.
// The candidate scan must still wake the unindexed adopters so one can recreate it.
//
// The artifact is deliberately absent from the client: on a Delete event the object is
// already gone, so the map function has only what it is handed.
func TestFindTemplateCachesForArtifact_WakesAdoptersAfterArtifactDeleted(t *testing.T) {
	deleted := newTestArtifact("hf---meta-llama-llama-3-1-8b-instruct-d4dc6f81cc")
	deleted.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	waiting := newTestTemplateCache("cache-waiting", aimv1alpha1.TemplateCacheModeShared)
	alsoWaiting := newTestTemplateCache("cache-also-waiting", aimv1alpha1.TemplateCacheModeShared)
	lagging := resolvingArtifact(newTestTemplateCache("cache-lagging", aimv1alpha1.TemplateCacheModeShared), deleted.Name)
	elsewhere := resolvingArtifact(newTestTemplateCache("cache-elsewhere", aimv1alpha1.TemplateCacheModeShared), "hf---mistralai-mistral-7b-fa03e1b7c2")

	r := newTemplateCacheTestReconciler(t, waiting, alsoWaiting, lagging, elsewhere)

	got := mappedTemplateCaches(t, r, deleted)
	want := []string{alsoWaiting.Name, elsewhere.Name, lagging.Name, waiting.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// The exact index keeps a stale resolver in the union even though it can no longer adopt
// the artifact; the candidate scan adds the current dedicated owner.
func TestFindTemplateCachesForArtifact_UnionsStaleResolverWithCurrentAdopter(t *testing.T) {
	claimed := newTestArtifact("hf---meta-llama-llama-3-1-8b-instruct-d4dc6f81cc")

	dedicated := newTestTemplateCache("cache-dedicated", aimv1alpha1.TemplateCacheModeDedicated)
	stranded := resolvingArtifact(newTestTemplateCache("cache-shared", aimv1alpha1.TemplateCacheModeShared), claimed.Name)
	// A Dedicated cache took ownership, which puts the artifact out of reach of every
	// Shared cache - including the one still publishing it.
	claimed.OwnerReferences = []metav1.OwnerReference{ownedByTemplateCache(dedicated)}

	r := newTemplateCacheTestReconciler(t, dedicated, stranded, claimed)

	got := mappedTemplateCaches(t, r, claimed)
	want := []string{dedicated.Name, stranded.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// Map functions run on the manager's watch goroutine, so an unchecked type assertion
// would take down the process rather than one reconcile. The predicate deliberately
// forwards objects it cannot inspect, which makes this the reachable end of that
// contract.
func TestFindTemplateCachesForArtifact_IgnoresNonArtifactObject(t *testing.T) {
	r := newTemplateCacheTestReconciler(t, newTestTemplateCache("cache-shared", aimv1alpha1.TemplateCacheModeShared))

	got := r.findTemplateCachesForArtifact(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "not-an-artifact", Namespace: templateCacheTestNamespace},
	})
	if got != nil {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want nil for a non-artifact object", got)
	}
}

// Shared caches deliberately share one artifact: generateArtifactName is not scoped
// to the cache, so two caches whose templates resolve the same source URI both track
// it. Only the cache that created the artifact carries the template-cache.name label,
// so a label-keyed mapping woke that one and left every other adopter waiting on a
// Ready transition it never saw.
func TestFindTemplateCachesForArtifact_WakesEveryAdopterOfSharedArtifact(t *testing.T) {
	creator := newTestTemplateCache("cache-mi300x-thr", aimv1alpha1.TemplateCacheModeShared)
	adopter := newTestTemplateCache("cache-mi300x-lat", aimv1alpha1.TemplateCacheModeShared)

	artifact := newTestArtifact("hf---meta-llama-llama-3-1-8b-instruct-d4dc6f81cc")
	artifact.Labels = map[string]string{constants.LabelTemplateCacheName: creator.Name}

	r := newTemplateCacheTestReconciler(t, creator, adopter, artifact)

	got := mappedTemplateCaches(t, r, artifact)
	want := []string{adopter.Name, creator.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// An artifact a template cache is forbidden to adopt must not wake it, in either
// direction: shared caches only use unowned artifacts, dedicated caches only their own.
func TestFindTemplateCachesForArtifact_RespectsModeIsolation(t *testing.T) {
	shared := newTestTemplateCache("cache-shared", aimv1alpha1.TemplateCacheModeShared)
	dedicated := newTestTemplateCache("cache-dedicated", aimv1alpha1.TemplateCacheModeDedicated)
	otherDedicated := newTestTemplateCache("cache-dedicated-other", aimv1alpha1.TemplateCacheModeDedicated)

	sharedArtifact := newTestArtifact("artifact-shared")
	dedicatedArtifact := newTestArtifact("artifact-dedicated")
	dedicatedArtifact.OwnerReferences = []metav1.OwnerReference{ownedByTemplateCache(dedicated)}

	r := newTemplateCacheTestReconciler(t, shared, dedicated, otherDedicated, sharedArtifact, dedicatedArtifact)

	if got, want := mappedTemplateCaches(t, r, sharedArtifact), []string{shared.Name}; !slices.Equal(got, want) {
		t.Errorf("unowned artifact mapped to %v, want %v", got, want)
	}
	if got, want := mappedTemplateCaches(t, r, dedicatedArtifact), []string{dedicated.Name}; !slices.Equal(got, want) {
		t.Errorf("owned artifact mapped to %v, want %v", got, want)
	}
}

// A cache pinned to a storage class can never adopt an artifact on another one, so
// those events are dropped rather than costing a reconcile.
func TestFindTemplateCachesForArtifact_SkipsStorageClassMismatch(t *testing.T) {
	matching := newTestTemplateCache("cache-fast", aimv1alpha1.TemplateCacheModeShared)
	matching.Spec.StorageClassName = "fast"
	mismatched := newTestTemplateCache("cache-slow", aimv1alpha1.TemplateCacheModeShared)
	mismatched.Spec.StorageClassName = "slow"
	unpinned := newTestTemplateCache("cache-any", aimv1alpha1.TemplateCacheModeShared)

	artifact := newTestArtifact("artifact-fast")
	artifact.Spec.StorageClassName = "fast"

	r := newTemplateCacheTestReconciler(t, matching, mismatched, unpinned, artifact)

	got := mappedTemplateCaches(t, r, artifact)
	want := []string{unpinned.Name, matching.Name}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

// Artifacts created outside a template cache (user-authored, or from a release that
// predates the label) are still adoptable by a shared cache, so they must map too.
func TestFindTemplateCachesForArtifact_MapsUnlabelledArtifact(t *testing.T) {
	shared := newTestTemplateCache("cache-shared", aimv1alpha1.TemplateCacheModeShared)
	legacy := newTestTemplateCache("cache-legacy", "")

	artifact := newTestArtifact("artifact-hand-written")

	r := newTemplateCacheTestReconciler(t, shared, legacy, artifact)

	got := mappedTemplateCaches(t, r, artifact)
	want := []string{legacy.Name, shared.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}

func TestFindTemplateCachesForArtifact_IgnoresOtherNamespaces(t *testing.T) {
	local := newTestTemplateCache("cache-local", aimv1alpha1.TemplateCacheModeShared)
	remote := newTestTemplateCache("cache-remote", aimv1alpha1.TemplateCacheModeShared)
	remote.Namespace = "other-namespace"

	artifact := newTestArtifact("artifact-local")

	r := newTemplateCacheTestReconciler(t, local, remote, artifact)

	got := mappedTemplateCaches(t, r, artifact)
	want := []string{local.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("findTemplateCachesForArtifact() = %v, want %v", got, want)
	}
}
