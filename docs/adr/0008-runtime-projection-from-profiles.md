# ADR 0008: Runtime Projection from Profiles (eager + lazy) and AIMService Runtime Consumption

**Date:** 2026-06-29

**Status:** Accepted

**Supersedes:** [ADR 0007](0007-hardware-aware-kserve-cluster-serving-runtimes.md)
(its §5 "separate CSR-projection reconciler" and §6 "AIMService stays inline
forever"). The motivation and field-shape of 0007 remain useful background; this
ADR is the authoritative design.

**Decision:** AIM Engine projects KServe serving runtimes from AIM profiles via
**two mechanisms** — **eager** projection owned by the profile reconcilers
(governed by a projection mode), and **lazy** projection driven by an
`InferenceService` watch (always on) — and **`AIMService` consumes those runtimes
by reference instead of inlining the predictor** (the inline path is retired).

---

## Context

AIM Engine deployed inference by inlining the full predictor (image, resources,
affinity, profile ConfigMap, framework env) into each `InferenceService`. ADR 0007
proposed projecting `ClusterServingRuntime` objects from `AIMClusterProfile` for
native-KServe portability, in a *separate* CSR reconciler, and explicitly left the
`AIMService` inline path untouched.

Designing the implementation surfaced two facts that reshape that proposal:

1. **The profile reconcilers already compute everything a runtime needs** (image,
   resolved resources, node affinity, deployability, framework env is purely
   profile-derived), and the reconcile pipeline already does SSA + ownerRef + GC.
   A separate projection reconciler would re-fetch and re-derive all of it.
2. **Profile ConfigMaps are namespaced.** A namespace `ServingRuntime` can colocate
   its ConfigMap; a cluster `ClusterServingRuntime` cannot guarantee one in an
   arbitrary consumer namespace. The common enterprise layout — a namespaced
   `AIMService` referencing a cluster profile — needs a *complete* runtime in the
   consumer's namespace, which is only known by watching `InferenceService`s.

## Decision details

- **Eager projection (mode-governed).** Each profile reconciler projects a runtime
  for its profile: `AIMClusterProfile` → `ClusterServingRuntime`,
  `AIMProfile` → `ServingRuntime`.   Owned by the profile (same-scope), SSA + GC by
  the pipeline. Governed by a runtime-neutral **projection mode**: `Exhaustive`
  (per-profile runtimes, named `aim-<profile.Name>`, `autoSelect` off), `Reduced`
  (one model-slug primary per model, referenced by name), or both. Every
  projected runtime keeps `autoSelect` off: all runtimes share the single
  `huggingface` model format, so `autoSelect` on any of them would collide across
  unrelated models (and hijack any co-installed generic `huggingface` runtime) in
  KServe's format-based auto-selection. Native consumers reference the model-slug
  primary explicitly by name, which works regardless of `autoSelect`.
- **Lazy projection (always on, mode-independent).** An `InferenceService` watch
  materializes a **complete** namespace `ServingRuntime` (+ colocated ConfigMap) in
  the ISVC's namespace when the referenced runtime isn't already complete there.
  Resolution is **annotation-free for the native flow**: a plain KServe ISVC
  referencing a managed CSR is completed by following the **CSR's ownerRef/label →
  backing `AIMClusterProfile`**. An AIMService-stamped profile annotation is an
  optional fast-path for the "runtime not yet created" case (cross-scope or
  `Reduced`). Resolution is **profile-scope-aware**: the annotation and name-lookup
  paths resolve **either** a namespace `AIMProfile` in the ISVC's namespace **or** a
  cluster `AIMClusterProfile`, namespace-first (matching the `AIMService` resolver's
  precedence), so the materialized runtime can be owned by whichever profile scope
  actually backs the service. This is what makes (i) a namespaced `AIMService` →
  cluster profile work, (ii) a native KServe ISVC referencing a CSR get its profile
  ConfigMap, (iii) an `AIMService`'s runtime exist regardless of the eager mode, and
  (iv) a `Reduced`-mode namespace-`AIMProfile`-backed `AIMService` self-complete even
  though no eager per-profile runtime exists (see the post-review addendum). Completion
  is delivered by **shadowing the bare CSR with a complete namespace SR** (the CSR is
  the cluster-scoped, native-referenceable handle the operator completes per-namespace,
  not a standalone-servable runtime), which relies on KServe resolving a namespace SR over a
  same-named CSR — confirmed behaviour (see the
  [spike-findings appendix](#appendix-kserve-runtime-resolution-spike-findings),
  finding 2).
- **`AIMService` always references, never inlines.** The ISVC sets
  `predictor.model.runtime: aim-<profile.Name>` and overlays only service-specific
  fields (service-owned cache + redirect env, autoscaling, auth, service-level
  resource / pull-secret / service-account overrides). The runtime carries the
  full profile-derived **framework env** and the profile-owned cache mount. The
  reference is **sticky** (not re-evaluated/flipped on a mode change).
- **Projectable = `deployable && image != "" && matchingNodes > 0`. Teardown is
  asymmetric:** creation is gated, but an existing runtime is never deleted when the
  gate later flips (e.g. nodes vanish) — it is kept and marked `Degraded`. Removal
  happens only on profile delete or explicit disable. Implementation is **purely
  additive** (see the 2026-07-06 addendum): projection emits the runtime only while
  projectable and simply stops emitting it on a gate flip; the apply pipeline never
  prunes owned objects absent from the plan, so the runtime survives without any
  re-apply keep-alive. The `Degraded` signal is derived from the profile's prior
  `RuntimeProjected` condition, not a per-reconcile runtime-existence `Get`.
- **Reserved `aim-` prefix + authoritative apply.** Every projected runtime is
  named `aim-<profile.Name>`, a prefix AIM Engine owns exclusively. Because the
  name can never collide with a hand-authored runtime, AIM Engine always
  **force-applies** (SSA + `ForceOwnership`) the runtimes it projects —
  unconditionally, with no pre-apply `Get`, no confirmed-ours/absent/foreign
  branching, and no `RuntimeNameConflict`. SSA force reconciles drift on the fields
  AIM Engine owns while preserving additive fields set only by other managers, and
  removes the infinite-requeue on our own objects. Hand-authored runtimes use any
  non-`aim-` name and are never read, adopted, or modified.

## Consequences

- **Positive:** native KServe and `AIMService` converge on one runtime object
  (DRY); the common cross-scope layout is supported; the eager mode tunes native
  discoverability without affecting `AIMService` correctness; profiles self-heal
  runtime drift via existing Node/owned-object watches.
- **Negative / costs:** the lazy `InferenceService`-watch reconciler is the hardest
  piece (cross-scope ownership, cleanup). MVP accepts lingering lazy runtimes (GC'd
  on profile delete); a last-consumer refcount sweep is deferred.
- **KServe resolution invariants (confirmed — see the
  [spike-findings appendix](#appendix-kserve-runtime-resolution-spike-findings)).**
  Three KServe behaviours this design
  depends on were validated empirically against KServe v0.16.0 (the same v0.16 minor
  the repo targets at v0.16.1; the resolution/merge logic is unchanged across the
  patch line) before the AIMService rewrite landed. They remain load-bearing
  invariants:
  1. KServe merges ISVC `predictor.model` env over the runtime container's env **by
     name**, with the ISVC winning on conflict (finding 1) — the service-owned-cache
     env override depends on it.
  2. An explicit `predictor.model.runtime: <name>` resolves a namespace
     `ServingRuntime` **before** a same-named `ClusterServingRuntime` (finding 2) —
     what makes shadow-completion viable.
  3. Deleting a referenced runtime under a running ISVC leaves the existing
     Deployment/pod intact; KServe only flips `modelStatus` to
     `InvalidSpec`/`FailedToLoad`/`RuntimeNotRecognized` and never yanks the live
     workload (finding 3) — what makes asymmetric teardown safe.

See [`../../CONTEXT.md`](../../CONTEXT.md) for the ubiquitous language, and the
[spike-findings appendix](#appendix-kserve-runtime-resolution-spike-findings) below
for the KServe runtime-resolution behaviours this design depends on.

---

## Post-implementation review addenda

### 2026-07-06 — Lazy resolver is profile-scope-aware (Reduced + namespace profiles)

**Decision: Option A (support it).** The post-implementation review found that the
lazy `InferenceService`-watch resolver was originally `AIMClusterProfile`-only: all
three resolution steps (`profileFromManagedClusterRuntime`, `profileFromAnnotation`,
`profileFromNameLookup`) returned only a cluster profile, the fast-path
`runtime-profile` annotation was stamped for cluster scope only, and
`ProjectionState.BackingProfile()` was typed `*AIMClusterProfile`. Under
`--runtime-projection-mode=Reduced` a namespace `AIMProfile` gets **no** eager
per-profile `ServingRuntime` (`ProjectsPerProfile()` is false), so a namespaced
`AIMService` backed by that namespace profile emitted an ISVC referencing
`aim-<profile>` that could never be rescued — it sat at `InvalidSpec` /
`RuntimeNotRecognized` indefinitely, contradicting decision 5's "regardless of the
eager mode" guarantee (PRD user story 25).

We chose **Option A** (make the lazy resolver profile-scope-aware) over Option B
(reject `Reduced` + namespace profiles at startup), because the shared
`serving.BuildNamespaceServingRuntime` builder already handles both scopes — the gap
was purely in *resolution* — and Option B would leave a documented mode knob that
silently bricks a class of services. Concretely:

- `BackingProfile` is now a scope-agnostic interface (`client.Object` +
  `GetProfileSpecCommon()` + `GetStatus()`) implemented by both `AIMClusterProfile`
  and `AIMProfile`; `ProjectionState`'s annotation/name-lookup candidates and
  `DesiredProjection.Owner` carry either scope.
- The annotation and name-lookup paths resolve namespace-first in the ISVC's
  namespace (a namespace `AIMProfile`), then fall back to a cluster
  `AIMClusterProfile`. The fast-path annotation is now stamped for namespace-scope
  services too.
- A namespace-`AIMProfile`-backed lazy shadow carries the **same** `AIMProfile`
  ownerReference as that profile's *eager* per-profile projection would, so the
  self-heal "a managed shadow is never complete" rule can no longer distinguish them
  by ownerRef kind (the earlier cluster-profile self-heal used
  `ownedByClusterProfile`). The shadow is now marked
  with a dedicated label (`aim.eai.amd.com/runtime-projection=lazy`) that only the
  lazy projection stamps; self-heal keys off that marker, so eager projections and
  hand-authored runtimes are still correctly deferred to.
- A namespace-`AIMProfile` watch + namespace-scoped mapping handler self-heal a
  namespace-profile shadow (mirroring the `AIMClusterProfile` watch), and the
  profile-cache lookup/handler now match the backing profile's scope.

This makes decision 5's guarantee true for all profile scopes and modes. The
model-slug lazy completion extends the single `runtimeNameMatchesProfile` name
guard alongside this change without touching the resolver's control flow.

### 2026-07-06 — Asymmetric teardown is additive (drop the `runtimeExists` keep-alive)

**Decision: go-additive.** The original eager projection implemented asymmetric
teardown with a `runtimeExists` keep-alive: `FetchRemoteState` did an extra per-
reconcile `Get` on `aim-<profile.Name>` (via `namespaceRuntimeExists` /
`clusterRuntimeExists`, gated on `ProjectsPerProfile()`), stored the result on
`ProfileFetchResult.runtimeExists` / `ClusterProfileFetchResult.runtimeExists`, and
`planNamespaceRuntime` / `planClusterRuntime` **re-emitted** the runtime when
`!projectable && runtimeExists` to "keep it alive". Plan decision 10 justified this
as *"the builder cannot be a pure return-nil→GC function."*

The post-implementation review found that premise is **false for this pipeline**.
The apply loop (`internal/controller/utils/reconciler.go` Phase 5) deletes only the
objects a reconciler explicitly places in `plan.Delete(...)`; it performs **no
diff-based pruning** of owned objects that are simply absent from the plan. Owned
runtimes are garbage-collected purely by Kubernetes ownerRef when the parent profile
is deleted. So non-emission already preserves an existing runtime — the re-emit
keep-alive defended against a deletion this pipeline cannot perform.

We therefore made projection **purely additive**, matching every other resource in
the pipeline:

- `projectable` → force-apply the runtime (create/update). Unchanged.
- `!projectable` → emit nothing. The runtime survives via non-pruning; it is removed
  only by ownerRef GC on profile delete or an explicit `plan.Delete(...)` on disable.
- The `RuntimeProjected=Degraded` signal is preserved but now derives the
  "was a runtime projected before" fact from the profile's **prior**
  `RuntimeProjected` condition (the `ConditionManager` is seeded from existing
  status before decoration runs), so `Degraded` fires exactly when the last reconcile
  had `RuntimeProjected=True` and the gate has since flipped — no runtime-existence
  `Get`.

This removed the two `runtimeExists` fields, the two `*RuntimeExists` helpers, the
extra `Get`, and the keep-alive branch, while preserving observable behaviour
(runtime survives a gate flip; condition goes `False`/`RuntimeDegraded`). The
model-slug primary path (Reduced) is unchanged: its survival was never tracked, so
`decorateProjectionCondition` passes `wasProjected=false` for that path and it stays
`True`-or-silent (onPrimaryUnavailable degrade remains deferred). The one deliberate
behavioural narrowing: if a runtime exists but the profile's `RuntimeProjected`
condition was never `True` (e.g. an out-of-band runtime under the reserved prefix),
the additive path stays silent instead of marking `Degraded` — consistent with the
review's finding that "existed before" lives on the profile's own status.

The alternative (keep the code, re-justify decision 10 as *defensive against a
hypothetical future pruning pipeline*) was rejected: it retains an implicit
dependency and an untested branch to guard a threat that does not exist today. If
diff-based pruning is ever added to the pipeline, runtime survival on a gate flip
must be re-established explicitly (an `plan`-level keep or a pruning allowlist), and
this addendum is the pointer to that requirement.

### 2026-07-08 — Upgrading an already-running service: in-place inline→reference ISVC transition + orphan ConfigMap GC

**Decision: convergence + document the transient window (Option A), do not gate.**
Every chainsaw test creates *fresh* services, so the upgrade path — a service that
was already running under the retired inline predictor when the operator is
upgraded to the runtime-reference build — was untested. Two hazards were reviewed:

1. **Orphaned service-owned ConfigMap.** The retired inline path built a
   per-service profile ConfigMap named `<service>-profile-<hash>` (via the removed
   `profileConfigMapName` / `buildProfileConfigMap`), owner-ref'd by the
   `AIMService`. The runtime-reference reconcile never plans it, and the apply
   pipeline does no diff-based pruning (see the addendum above), so it would linger
   for the life of the service (GC'd only on service delete). The reconcile Plan
   step now GCs it explicitly with a strictly-guarded `plan.Delete`: it deletes the
   ConfigMap only when it is fetched by the deterministic legacy name (name shape)
   **and** owner-ref'd by this `AIMService` (UID match) **and** carries the
   `app.kubernetes.io/managed-by: aim-engine` label. All three guards must hold, so
   a coincidentally-named user ConfigMap is never touched; a false-positive delete
   of a user object would be the worst-case regression. The delete is idempotent
   (NotFound-tolerant), so it is a no-op once reclaimed and for any service created
   after the rewrite. See `internal/v1alpha2/aimservice/reconcile.go`
   (`planLegacyProfileConfigMapCleanup`) and `configmap.go`
   (`legacyProfileConfigMapName`, recovered verbatim from git history so the target
   matches the retired derivation exactly).

2. **In-place inline→reference ISVC rewrite.** The new reconcile re-applies the
   *same-named* `InferenceService`, switching `predictor.containers[0]` (inline) →
   `predictor.model.runtime` (reference) in place. Because both the retired and the
   new reconcile apply the ISVC under the same SSA field manager
   (`aim-service-controller`), the re-apply that omits `containers` correctly drops
   the inline predictor and adds the runtime reference — SSA removes fields the
   manager previously owned and no longer sends. `stickyRuntimeName` reads the
   *existing* `predictor.model.runtime`, which is empty on an inline ISVC, so it
   falls through to `serving.RuntimeName(profileName)` (the hashed per-profile
   name) — the migration lands on the correct reference. For a cluster-profile-
   backed service the referenced runtime is materialized by the always-on lazy
   `InferenceService`-watch reconciler *after* the rewrite (the ISVC carries the
   `runtime-profile` annotation fast-path), so there is a brief ordering window
   where the rewritten ISVC references a runtime that does not yet exist. We accept
   this window rather than gating the rewrite on runtime resolvability, because the
   [spike-findings appendix](#appendix-kserve-runtime-resolution-spike-findings)
   finding 3 shows KServe leaves a
   running predictor Deployment/pod **intact** when its referenced runtime is
   absent — it only flags `status.modelStatus` to
   `InvalidSpec`/`FailedToLoad`/`RuntimeNotRecognized`; it never yanks the live
   workload. The lazy watcher then materializes the runtime and the next KServe
   re-render binds it (`status.servingRuntimeName`). Gating the rewrite (Option B)
   would add ordering complexity to defend a live-pod teardown that finding 3 proves
   does not happen, so it was rejected.

Coverage: `internal/v1alpha2/aimservice/reconcile_test.go`
(`TestPlanResources_LegacyProfileConfigMapCleanup`, incl. the false-positive
negative cases) and the render-time chainsaw
`tests/e2e/v1alpha2/runtime-projection/service-upgrade-migration/` (default
`Exhaustive`/kind lane), which stands up a pre-upgrade inline ISVC + orphan
ConfigMap and asserts the reference rewrite, the KServe runtime binding, the
projected-ConfigMap mount, and the orphan deletion.

### 2026-07-08 — Bare CSR carries a dangling `AIM_PROFILE_ID` before the lazy shadow lands: the bare CSR is a shadow target in all cases

**Decision: Option A (document the lifecycle; do not guard).** The bare
`ClusterServingRuntime` that eager cluster projection emits
(`serving.BuildClusterServingRuntime`) carries the full profile-derived framework
env — including `AIM_PROFILE_ID=custom/<aimId>/<name>` — but **no** colocated
profile ConfigMap and **no** profile volume/mount (a cluster runtime cannot
guarantee a namespaced ConfigMap in an arbitrary consumer namespace). So on a
custom / derived / cache profile the CSR points `AIM_PROFILE_ID` at a file
(`/workspace/aim-runtime/profiles/custom/<aimId>/<name>.yaml`) it does not itself
mount. That file exists only once the lazy `InferenceService`-watch reconciler
materializes the namespace shadow (a complete namespace `ServingRuntime` +
colocated ConfigMap of the same name). This holds for **every** profile, not just
custom/derived/cache ones: `AIM_PROFILE_ID` always resolves under the
operator-owned `.../profiles/custom/` subtree, and **no image bakes profiles
there** — image-baked profiles live at `.../profiles/<aim_id>/<profile_id>.yaml`
under a different filename convention. So the bare CSR is a **shadow target in all
cases**, not directly consumable until the shadow completes it; it is the
cluster-scoped, native-referenceable handle, not a standalone-servable runtime.
(The runtime's profile lookup short-circuits on an explicit `AIM_PROFILE_ID` with
no fallback to `AIM_ID` auto-selection, so even an image-baked model bound to a
bare CSR resolves the missing `custom/...` file rather than the baked profile.)

The **reconvergence window was measured** on the live GPU vcluster (the bare CSR
with a dangling `AIM_PROFILE_ID` present, then a native KServe ISVC referencing it
applied). The shadow ConfigMap + namespace `ServingRuntime` materialize within
**~1 s** of the ISVC create, and KServe re-renders the predictor onto the
namespace SR (`status.servingRuntimeName` set, `clusterServingRuntimeName`
cleared) within **~2.5–4 s** — with no manual intervention (see the
[spike-findings appendix](#appendix-kserve-runtime-resolution-spike-findings)
finding 4 for the exact runs). The window
is **bounded**: it is driven by the synchronous ISVC-create → lazy-reconcile →
force-apply shadow → KServe re-render chain, and the lazy reconciler additionally
re-enqueues on the backing-profile, profile-cache, and shadow-object watches with
controller-runtime requeue-on-error, so there is no ordering in which the
reference stays unresolved indefinitely.

We chose **Option A** (document only) over **Option B** (suppress the bare CSR or
mark it with a new "incomplete-runtime" state), because:

- The transient is **bounded and self-healing** (measured seconds), and the
  [spike-findings appendix](#appendix-kserve-runtime-resolution-spike-findings)
  finding 3 already shows KServe never
  yanks a live workload when a referenced runtime is incomplete/absent — it only
  flags `status.modelStatus`. So the worst case is a brief pre-shadow render, not
  a stuck service.
- Option B would either withhold the native-KServe-portable CSR that decision 1
  (DRY: native KServe and `AIMService` converge on one runtime object) exists to
  provide, or add a new "incomplete-runtime" marker/state for a hazard that
  self-heals in seconds — cost without a defect to defend against.
- The env↔ConfigMap-key **invariant makes the dangle self-correcting by
  construction**: both the eager bare CSR and the (eager/lazy) complete namespace
  runtime derive `AIM_PROFILE_ID` and the colocated ConfigMap's data key from the
  **same** `serving.ProfileFilename`. Discovered profiles use their authoritative
  `spec.profileId`; hand-authored profiles without one use the engine-aware
  fallback axes (engine, accelerator, precision, count, metric, optional
  variant). Neither path depends on the KServe runtime OBJECT name, so hashing
  that object name never changes
  `AIM_PROFILE_ID`, and once the shadow's same-key ConfigMap is mounted the env
  resolves. This invariant is load-bearing and must be preserved.

To keep the transient **observable/understandable** rather than silent, the
`BuildClusterServingRuntime` and `BuildFrameworkEnvVars` godocs now state this
lifecycle explicitly (a shadow target in all cases, completed per-namespace by the
lazy reconciler — never standalone-servable), and the shadow objects carry the full-fidelity profile
labels/annotations so a bare-CSR-bound ISVC and its materializing
shadow are traceable to the same backing profile.

Coverage: `internal/v1alpha2/serving/runtime_test.go`
(`TestBareCSRProfileID_MatchesShadowConfigMapKey`, the env↔ConfigMap-key
round-trip, incl. the assertion that it is independent of the hashed runtime
object name) and the render-time chainsaw
`tests/e2e/v1alpha2/runtime-projection/bare-csr-before-shadow/` (default
`Exhaustive`/kind lane), which applies the cluster profile + a bare CSR carrying
the dangling `AIM_PROFILE_ID`, then a native ISVC, and asserts the shadow SR +
ConfigMap materialize, the ISVC binds the **namespace** SR with the profile file
mounted, and the shadow's `AIM_PROFILE_ID` matches the ConfigMap data key.

---

## Appendix: KServe runtime-resolution spike findings

Validates the three KServe behaviours the AIMService rewrite + shadow-completion
depend on, plus the measured bare-CSR→lazy-shadow reconvergence window (finding 4).
These are the empirical basis for the "KServe resolution invariants" bullet under
[Consequences](#consequences) and the bare-CSR addendum above.

### Environment

- Cluster: a vcluster, `Standard` (raw) deployment mode, `autoscalerClass: external`.
- KServe controller: **`kserve/kserve-controller:v0.16.0`** (repo targets v0.16.1 —
  same minor; resolution/merge logic is unchanged across the v0.16 patch line).
- Findings 1–3 use `registry.k8s.io/pause` as the runtime image so the predictor
  Deployment renders without GPUs or model downloads. Pods reach `Running` but not
  `Ready` (pause serves no readiness endpoint); env/runtime resolution is fully
  decided at Deployment-render time, so readiness is irrelevant to these findings.

### Finding 1 — ISVC `predictor.model` env overrides runtime env by name (ISVC wins) — CONFIRMED

Runtime `spike-env-runtime` declares `SHARED_VAR=from-runtime`,
`RUNTIME_ONLY=runtime-value`; the ISVC `spike-env` declares `SHARED_VAR=from-isvc`,
`ISVC_ONLY=isvc-value`.

Rendered `spike-env-predictor` Deployment `kserve-container` env:

```json
[
  { "name": "SHARED_VAR",  "value": "from-isvc" },
  { "name": "ISVC_ONLY",   "value": "isvc-value" },
  { "name": "RUNTIME_ONLY", "value": "runtime-value" }
]
```

- `SHARED_VAR` resolves to `from-isvc` — the ISVC value **wins on conflict**, and
  the key is **merged by name** (single entry, not duplicated).
- `RUNTIME_ONLY` (runtime-only) is preserved; `ISVC_ONLY` (ISVC-only) is added.

**Implication:** the service-owned-cache override is viable — an overlay env on the
ISVC (e.g. a cache redirect) beats the runtime's env of the same name.

### Finding 2 — namespace `ServingRuntime` resolves before a same-named `ClusterServingRuntime` — CONFIRMED

Both a `ClusterServingRuntime` `spike-shadow` (image `pause:3.8`,
`ORIGIN=cluster-runtime`) and a namespace `ServingRuntime` `spike-shadow` (image
`pause:3.9`, `ORIGIN=namespace-runtime`) exist. The ISVC `spike-shadow` sets
`predictor.model.runtime: spike-shadow`.

Observed resolution:

- `status.servingRuntimeName = spike-shadow`, `status.clusterServingRuntimeName`
  is **empty** → the **namespace SR** was selected.
- Rendered Deployment image = `registry.k8s.io/pause:3.9` (the namespace SR's).
- Rendered `ORIGIN` env = `namespace-runtime`.

**Implication:** shadow-completion is **viable**. A complete namespace `ServingRuntime`
named `aim-<x>` transparently shadows a bare `ClusterServingRuntime` named `aim-<x>`
for any ISVC in that namespace; the CSR itself is the cluster-scoped handle the
operator completes per-namespace, not a standalone-servable runtime.
The escalation path in the acceptance criteria (CSR hard-referencing a per-namespace
ConfigMap) is **not** needed.

### Finding 3 — runtime deleted under a running ISVC: workload survives, spec flagged invalid — CONFIRMED

Two independent data points:

**(a) Pre-existing long-running ISVC** whose CSR had already been deleted before the
spike:

- The predictor Deployment is still `1/1` Ready.
- Top-level conditions stay `Ready=True`, `PredictorReady=True`, `IngressReady=True`.
- `status.modelStatus`: `transitionStatus=InvalidSpec`,
  `states.activeModelState=Loaded`, `targetModelState=FailedToLoad`,
  `lastFailureInfo.reason=RuntimeNotRecognized`
  ("Waiting for runtime to become available").

**(b) Clean reproduction:** apply a CSR + ISVC, wait for the pod `Running`, then
delete the `ClusterServingRuntime` and poke a re-reconcile (annotate the ISVC):

- The predictor pod stays `Running`; the Deployment is not deleted.
- `status.modelStatus`: `transitionStatus=InvalidSpec`,
  `targetModelState=FailedToLoad`, `lastFailureInfo.reason=RuntimeNotRecognized`.
- Top-level `Ready=False` here, but that is **pre-existing** — the `pause` pod never
  passes KServe's readiness probe, so `PredictorReady` was already `False` before the
  deletion. Case (a), with a genuinely Ready pod, shows `Ready` stays `True`.

**Conclusion:** deleting the referenced runtime does **not** tear down the running
Deployment/pod. KServe's re-reconcile only flips `modelStatus` to
`InvalidSpec`/`FailedToLoad`/`RuntimeNotRecognized`; the existing rollout is left
intact and its readiness is unaffected by the missing runtime. A *subsequent* change
that forces a Deployment re-render while the runtime is absent would fail to resolve
the runtime (FailedToLoad), but the live pod is never yanked.

**Implication for asymmetric teardown:** safe. When matching hardware disappears AIM
Engine keeps the runtime and marks it `Degraded` rather than deleting it — and even
if a runtime did vanish, KServe leaves the running ISVC's pod alone, so there is no
risk of a transient node change yanking a live workload.

### Finding 4 — bare-CSR→lazy-shadow reconvergence window is bounded and self-heals — MEASURED

Bounds the window during which a native KServe `InferenceService` that binds a
**bare** `ClusterServingRuntime` (carrying `AIM_PROFILE_ID` but **no** colocated
profile ConfigMap) resolves the profile against a not-yet-mounted file, before the
lazy `InferenceService`-watch reconciler materializes the namespace shadow.

- Cluster: live GPU vcluster, operator built + deployed via Tilt
  (`aim-engine-controller-manager`, ns `aim-system`).
- Reproduction: an `AIMClusterProfile` (aimId `Qwen/Qwen3-0.6B`, no accelerator,
  `modelSources` set) at `Ready`, plus a hand-authored **bare** CSR
  `aim-test-bare-csr-899eadd9` carrying the framework env
  (`AIM_PROFILE_ID=custom/Qwen/Qwen3-0.6B/vllm-none-bf16-tp0-latency`, cache-redirect
  env) and only the `dshm` volume (no profile ConfigMap volume). Then a native ISVC
  `native-bare-csr-consumer` referencing the CSR is applied at T0 and object
  creation timestamps + `status.servingRuntimeName` are polled.

Two consecutive runs (fresh namespace each):

| Signal | run 1 | run 2 |
| --- | --- | --- |
| shadow ConfigMap created (after T0) | +0 s | +1 s |
| shadow namespace `ServingRuntime` created (after T0) | +1 s | +1 s |
| KServe bound the namespace SR (`status.servingRuntimeName`, client-poll) | ~+4.0 s | ~+2.5 s |
| final `status.servingRuntimeName` / `clusterServingRuntimeName` | `aim-test-bare-csr-899eadd9` / *(empty)* | `aim-test-bare-csr-899eadd9` / *(empty)* |

In both runs the shadow's container `AIM_PROFILE_ID` equalled
`custom/Qwen/Qwen3-0.6B/vllm-none-bf16-tp0-latency` and the colocated ConfigMap's
single data key was `vllm-none-bf16-tp0-latency.yaml` — i.e. once the shadow lands,
the env resolves against the mounted file (env↔ConfigMap-key agreement, keyed on
`ProfileFilename`, not the hashed runtime object name).

**Conclusion:** the reconvergence window is a **few seconds (≤ ~4 s measured)** and
**self-heals with no manual intervention** — the ISVC-create event synchronously
drives the lazy reconcile, the namespace SR shadows the CSR (finding 2), and KServe
re-renders onto the complete namespace SR. The window is **bounded**: it is
edge-triggered by the ISVC create and re-enqueued by the backing-profile /
profile-cache / shadow-object watches with controller-runtime requeue-on-error, so
no ordering leaves the reference unresolved indefinitely. Combined with finding 3
(KServe never yanks a live pod for an incomplete runtime), a bare CSR bound before
its shadow is a recognized, transient, self-healing state — not a stuck service.

**Implication for the bare CSR:** the bare CSR is a **shadow target in all cases**
(its `AIM_PROFILE_ID` always names an operator-owned `custom/...` file no image
bakes), converging in seconds once the shadow lands. This is the basis for the
2026-07-08 bare-CSR addendum above (Option A: document-only).
