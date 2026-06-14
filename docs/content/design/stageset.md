---
title: "StageSet: Staged Deployments of ExternalArtifacts for Flux"
weight: 1
---

# Design: StageSet — Staged Deployments of ExternalArtifacts for Flux

| | |
|---|---|
| **Status** | Draft v2 |
| **Kind** | `StageSet` (`stages.metio.wtf/v1`) |
| **Author** | Seb |
| **Last updated** | 2026-06-11 |
| **Changes from v1** | ExternalArtifact-only sources; `stages.metio.wtf/v1` group; per-stage `postBuild`; sharded `StageInventory` CRD instead of in-status inventory; `dependsOn` between StageSets; Flagger integration; typed stage actions replacing verification Jobs; ApplySet (KEP-3659) compliant labeling; all open questions resolved (`--inventory-shard-cap`, `--inventory-mode`, `--allowed-action-hosts`, `applyHelmHookResources`); producer-aware sourceRef resolution; per-resource conflict policies; `delete` and `apply` action verbs; versioned migrations; `rollbackOnFailure` |

## Summary

`StageSet` is a Flux-compatible controller and CRD that deploys an ordered
list of **stages**. Each stage consumes exactly one
**`ExternalArtifact`** (RFC-0012) — composed elsewhere, typically by
source-watcher's `ArtifactGenerator`, or produced by any third-party source
controller such as JaaS. A stage may reference the artifact directly or,
via **producer-aware resolution**, reference the higher-level object that
produced it (e.g. a JaaS `JsonnetSnippet`), resolved through the
artifact's RFC-0012 `spec.sourceRef` back-pointer. Stages are applied sequentially; each stage must pass its
**ready checks** before the next stage begins. Each stage owns its own
**inventory** and is **pruned independently** (default: enabled), including
reverse-order teardown of stages removed from the spec. Each stage may carry
**Kustomize patches** and **post-build variable substitution**, applied to
the rendered resources before server-side apply. Stages additionally
support typed **pre/post/onFailure actions** (`patch`, `http`, `wait`,
`job`) — a declarative replacement for Helm hook Jobs covering cases such
as toggling an application's maintenance mode around a rollout.

The mental model is "Helm hooks as a first-class citizen": ordering, gating,
and lifecycle are part of the API surface rather than annotations bolted onto
templates.

## Motivation

Flux users who need ordered rollouts today reach for one of two patterns,
both of which fall short:

1. **Chained `Kustomization` resources via `dependsOn`.** This expresses
   ordering, but each Kustomization reconciles its source independently.
   There is no atomic revision set: during a rollout, stage 1 may already be
   at revision *B* while stage 3 picks up an even newer revision *C*. The
   dependency graph is scattered across N custom resources, status must be
   aggregated manually, and removing a stage means deleting a CR — with no
   guarantee about teardown order relative to other stages.

2. **Helm hooks (`pre-install`, `post-upgrade`, …).** Hooks provide ordering
   inside a single release, but they are an afterthought of the templating
   layer: hook resources are exempt from normal release inventory tracking,
   weights are stringly-typed annotations, failures leave orphans, and the
   mechanism is unavailable to plain-manifest workloads.

`StageSet` provides:

- **One CR, one status, one revision set.** All `ExternalArtifact` revisions
  are resolved and pinned at the start of a reconciliation run; every stage
  in that run applies content from the pinned snapshot.
- **Ordering as API.** Stages are an ordered list in `spec`, not a graph
  reconstructed from `dependsOn` edges or hook-weight annotations.
- **Gated progression.** A stage completes only when its ready checks pass:
  kstatus by default, CEL expressions for custom resources, optional
  Job-based verification probes.
- **Per-stage pruning with reverse-order teardown**, backed by sharded
  inventory objects with no practical size limit.
- **Post-build patching and substitution per stage**, enabling fine-tuning
  of third-party content without forking it.

### Why ExternalArtifact-only?

v1 of this design accepted all five Flux source kinds per stage. Pivoting to
`ExternalArtifact` as the sole source kind is a deliberate separation of
concerns:

- **Composition is someone else's job.** ArtifactGenerator already composes
  GitRepository, OCIRepository, Bucket, HelmChart, and ExternalArtifact
  sources into deployable artifacts, including monorepo decomposition and
  path filtering. Re-implementing multi-artifact stages inside StageSet
  would duplicate that machinery with worse ergonomics.
- **RFC-0012 as the interoperability seam.** Any controller that produces an
  `ExternalArtifact` — Flux's own source-watcher, a CI-driven build
  controller, an in-house manifest renderer — works with StageSet without
  code changes. The digest-verification and readiness contract is defined by
  the RFC, not by us.
- **A radically thinner controller.** One watched source kind, one
  source-to-StageSet index, one artifact-fetch code path. The genuinely new
  code is the stage state machine, per-stage inventory, and revision
  pinning — exactly the value-add.

The practical consequence: for the common "one Git repo, several paths"
setup, the user declares one `ArtifactGenerator` emitting one
`ExternalArtifact` per stage, plus one `StageSet`. Source-watcher
(`flux bootstrap --components-extra=source-watcher`) becomes a de-facto
prerequisite for that workflow; this is documented, not hidden.

Note the layering: "ExternalArtifact-only" describes the **data plane**
(what bytes are fetched, verified, and built). The **reference layer** is
producer-aware (see Producer-aware source resolution): a stage may name the
producing object instead of the artifact, but resolution always lands on an
ExternalArtifact, so the single fetch/verify/build path is preserved.

### Helm delegation

StageSet never renders Helm templates and never executes Helm lifecycle
operations. The canonical pattern for sequencing Helm content is:

1. A stage's artifact contains a **`HelmRelease` manifest** (plus any
   supporting objects). StageSet applies it like any other resource.
2. The stage's ready check gates on the HelmRelease via CEL:

   ```yaml
   readyChecks:
     exprs:
       - apiVersion: helm.toolkit.fluxcd.io/v2
         kind: HelmRelease
         current: status.conditions.filter(c, c.type == 'Ready').exists(c, c.status == 'True')
         failed:  status.conditions.filter(c, c.type == 'Ready').exists(c, c.status == 'False')
   ```

3. helm-controller owns the release lifecycle (install, upgrade, rollback,
   tests, hooks); StageSet owns *when* the release participates in the
   rollout and *what depends on it*.

Note for users: composing a `HelmChart` source through ArtifactGenerator
yields the *unrendered* chart files — that is not a way to deploy a chart
with StageSet. The HelmRelease pattern above is.

Pre-rendered manifests carrying `helm.sh/hook` annotations are applied as
ordinary resources with a warning Event by default; per-stage
`applyHelmHookResources: false` strips them at BUILD instead.

## Goals

- Ordered, gated, multi-stage application of resources from
  `ExternalArtifact` sources.
- Per-stage inventory tracking with no practical size limit, pruning
  (default `true`), reverse-order teardown of removed stages and on object
  deletion.
- Custom readiness verification per stage: kstatus, CEL health-check
  expressions (`healthCheckExprs`-compatible shape), explicit object checks.
- Typed pre/post/onFailure actions per stage (`patch`, `http`, `wait`,
  `job`) with controller-owned idempotency.
- Kustomize patches and post-build variable substitution
  (`substitute`/`substituteFrom`) per stage.
- `dependsOn` between StageSet objects with kustomize-controller-compatible
  semantics.
- Multi-tenancy parity with kustomize-controller: impersonation via
  `serviceAccountName`, remote clusters via `kubeConfig`,
  `--no-cross-namespace-refs` support.
- Standard Flux operational behavior: reconcile-annotation triggers,
  suspend/resume, Events, Prometheus metrics, notification-controller
  compatibility.

## Non-Goals

- Source fetching or composition. ArtifactGenerator and third-party
  RFC-0012 producers own that.
- Helm rendering or lifecycle. helm-controller owns that; see the
  delegation pattern above.
- DAG / parallel execution *within* a StageSet in the first release. Stages
  are a strict sequence. Cross-StageSet fan-out is available via
  `dependsOn`.
- Progressive delivery (canary/blue-green). Flagger's job.

## API Design

### Example

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: platform
  namespace: flux-system
spec:
  interval: 10m
  retryInterval: 2m
  timeout: 5m                       # default per-stage timeout, overridable per stage
  serviceAccountName: platform-reconciler
  # kubeConfig: { secretRef: { name: prod-kubeconfig } }   # optional remote cluster

  dependsOn:                        # kustomize-controller-compatible semantics
    - name: cluster-bootstrap       # StageSet refs (same namespace unless set)

  stages:
    - name: crds
      sourceRef:
        name: platform-crds         # kind is always ExternalArtifact
      prune: false                  # CRDs: opt out of pruning
      readyChecks:
        timeout: 2m                 # kstatus on the applied set is the default

    - name: operators
      sourceRef:
        name: operators-bundle      # e.g. produced by an ArtifactGenerator
      path: ./manifests
      patches:
        - patch: |
            apiVersion: apps/v1
            kind: Deployment
            metadata:
              name: cert-manager
            spec:
              template:
                spec:
                  nodeSelector:
                    workload: platform
          target:
            kind: Deployment
            name: cert-manager
        - patch: |
            - op: replace
              path: /spec/replicas
              value: 3
          target:
            kind: Deployment
            name: ingress-nginx-controller
      postBuild:
        substitute:
          cluster_name: "prod-fra-1"
        substituteFrom:
          - kind: ConfigMap
            name: cluster-vars
      readyChecks:
        timeout: 5m
        exprs:
          - apiVersion: cert-manager.io/v1
            kind: ClusterIssuer
            current: status.conditions.filter(c, c.type == 'Ready').all(c, c.status == 'True')
            failed:  status.conditions.filter(c, c.type == 'Ready').all(c, c.status == 'False')

    - name: databases               # Helm delegation pattern: the artifact
      sourceRef:                    # contains HelmRelease manifests
        name: database-releases
      readyChecks:
        timeout: 10m
        exprs:
          - apiVersion: helm.toolkit.fluxcd.io/v2
            kind: HelmRelease
            current: status.conditions.filter(c, c.type == 'Ready').exists(c, c.status == 'True')
            failed:  status.conditions.filter(c, c.type == 'Ready').exists(c, c.status == 'False')

    - name: dashboards                # producer-aware reference: resolves to
      sourceRef:                        # the ExternalArtifact whose
        apiVersion: jaas.metio.wtf/v1   # spec.sourceRef points at this
        kind: JsonnetSnippet            # JsonnetSnippet (RFC-0012 back-pointer)
        name: grafana-dashboards
      readyChecks:
        timeout: 3m

    - name: webapp
      sourceRef:
        name: webapp-bundle
      actions:
        pre:
          - name: maintenance-on        # flip Ingress to the maintenance page
            patch:
              target:
                apiVersion: networking.k8s.io/v1
                kind: Ingress
                name: webapp
                namespace: webapp
              type: json6902
              patch: |
                - op: replace
                  path: /spec/rules/0/http/paths/0/backend/service/name
                  value: maintenance-page
          - name: wait-for-drain        # wait until sessions drained
            wait:
              target:
                apiVersion: metio.wtf/v1
                kind: WebApp
                name: webapp
                namespace: webapp
              expr: status.activeSessions == 0
              timeout: 5m
        post:
          - name: smoke-test            # ex-verificationJobs, now an action
            job:
              sourceRef:
                name: smoke-tests
              path: ./verify
          - name: maintenance-off
            patch:
              target:
                apiVersion: networking.k8s.io/v1
                kind: Ingress
                name: webapp
                namespace: webapp
              type: json6902
              patch: |
                - op: replace
                  path: /spec/rules/0/http/paths/0/backend/service/name
                  value: webapp
        onFailure:                      # rollout failed: keep maintenance on,
          - name: page-the-humans       # tell someone
            http:
              url: https://events.pagerduty.com/v2/enqueue
              method: POST
              bodyFrom:
                secretRef:
                  name: pagerduty-payload
      readyChecks:
        timeout: 10m
        checks:
          - apiVersion: apps/v1
            kind: Deployment
            name: webapp
            namespace: webapp

    - name: apps
      sourceRef:
        name: apps-production
      prune: true                   # explicit; true is the default
      readyChecks:
        timeout: 10m
        checks:                     # explicit objects, kstatus-evaluated
          - apiVersion: apps/v1
            kind: Deployment
            name: billing-api
            namespace: billing
```

### Types (abridged)

```go
// +groupName=stages.metio.wtf
// version: v1

type StageSetSpec struct {
    Interval           metav1.Duration            `json:"interval"`
    RetryInterval      *metav1.Duration           `json:"retryInterval,omitempty"`
    Timeout            *metav1.Duration           `json:"timeout,omitempty"`
    Suspend            bool                       `json:"suspend,omitempty"`
    DependsOn          []meta.NamespacedObjectReference `json:"dependsOn,omitempty"` // StageSet refs
    ServiceAccountName string                     `json:"serviceAccountName,omitempty"`
    KubeConfig         *meta.KubeConfigReference  `json:"kubeConfig,omitempty"`
    Stages             []Stage                    `json:"stages"` // minItems=1, names unique
}

type Stage struct {
    Name        string                       `json:"name"` // DNS-1123 label, inventory key
    SourceRef   SourceReference              `json:"sourceRef"`
    Path        string                       `json:"path,omitempty"`  // default "./"
    Prune       *bool                        `json:"prune,omitempty"` // default true
    Timeout     *metav1.Duration             `json:"timeout,omitempty"`
    Force       bool                         `json:"force,omitempty"` // sugar for conflictPolicy.default: Recreate
    ConflictPolicy *ConflictPolicy           `json:"conflictPolicy,omitempty"` // see Conflict Policies
    Patches     []kustomize.Patch            `json:"patches,omitempty"`     // fluxcd/pkg/apis/kustomize
    ApplyHelmHookResources *bool             `json:"applyHelmHookResources,omitempty"` // default true; false strips helm.sh/hook resources at BUILD
    PostBuild   *PostBuild                   `json:"postBuild,omitempty"`   // substitute / substituteFrom
    Actions     *StageActions                `json:"actions,omitempty"`
    ReadyChecks *ReadyChecks                 `json:"readyChecks,omitempty"`
}

type StageActions struct {
    Pre       []Action `json:"pre,omitempty"`       // before BUILD; failure aborts the stage untouched
    Post      []Action `json:"post,omitempty"`      // after VERIFY; stage is Ready only when these succeed
    OnFailure []Action `json:"onFailure,omitempty"` // best-effort, on any failure from APPLY onward
}

// Exactly one of Patch, HTTP, Wait, Job per action (CEL-validated oneOf).
type Action struct {
    Name    string           `json:"name"`
    Timeout *metav1.Duration `json:"timeout,omitempty"`
    Retries *int32           `json:"retries,omitempty"`
    Patch   *PatchAction     `json:"patch,omitempty"`
    HTTP    *HTTPAction      `json:"http,omitempty"`
    Wait    *WaitAction      `json:"wait,omitempty"`
    Job     *JobAction       `json:"job,omitempty"`
}

type PatchAction struct { // patch an existing in-cluster object (impersonated SA)
    Target meta.NamespacedObjectKindReference `json:"target"`
    Type   string                             `json:"type,omitempty"` // merge | json6902 (default merge)
    Patch  string                             `json:"patch"`
}

type HTTPAction struct {
    URL            string                   `json:"url"`
    Method         string                   `json:"method,omitempty"` // default POST
    Body           string                   `json:"body,omitempty"`
    BodyFrom       *meta.SecretKeyReference `json:"bodyFrom,omitempty"`
    HeadersFrom    []meta.SecretKeyReference `json:"headersFrom,omitempty"`
    ExpectedStatus []int32                  `json:"expectedStatus,omitempty"` // default 2xx
}

type WaitAction struct { // fixed duration, or CEL condition on an object
    Duration *metav1.Duration                    `json:"duration,omitempty"`
    Target   *meta.NamespacedObjectKindReference `json:"target,omitempty"`
    Expr     string                              `json:"expr,omitempty"`
    Timeout  *metav1.Duration                    `json:"timeout,omitempty"`
}

type JobAction struct { // render Jobs from an ExternalArtifact path and await completion
    SourceRef SourceReference           `json:"sourceRef"`
    Path      string                    `json:"path,omitempty"`
}

type DeleteAction struct { // delete an object so the next apply recreates it
    Target meta.NamespacedObjectKindReference `json:"target"`
    // IgnoreNotFound makes a missing target a no-op (default true).
    IgnoreNotFound *bool `json:"ignoreNotFound,omitempty"`
}

// SourceReference names either an ExternalArtifact directly (the default
// when Kind/APIVersion are omitted) or the producer object behind one,
// resolved via the artifact's RFC-0012 spec.sourceRef back-pointer.
type SourceReference struct {
    APIVersion string `json:"apiVersion,omitempty"` // default source.toolkit.fluxcd.io/v1
    Kind       string `json:"kind,omitempty"`       // default ExternalArtifact
    Name       string `json:"name"`
    Namespace  string `json:"namespace,omitempty"` // gated by --no-cross-namespace-refs
}

type ReadyChecks struct { // purely observational; active steps live in Actions
    Timeout     *metav1.Duration                     `json:"timeout,omitempty"`
    DisableWait bool                                 `json:"disableWait,omitempty"`
    Checks      []meta.NamespacedObjectKindReference `json:"checks,omitempty"`
    Exprs       []CustomHealthCheck                  `json:"exprs,omitempty"` // healthCheckExprs-compatible
}
```

Design notes:

- **Reuse over invention.** `kustomize.Patch` and `PostBuild` come from
  `fluxcd/pkg`, with semantics identical to `Kustomization` — users copy
  existing patches and CEL expressions verbatim.
- **Default readiness = kstatus on the stage's applied set** (the
  equivalent of `wait: true`). `disableWait: true` opts a stage out
  (useful for namespaces/RBAC-only stages).
- **Actions over hook Jobs.** Pre/post/onFailure steps are typed verbs
  (`patch`, `http`, `wait`, `job`) executed natively by the controller —
  see Stage Actions below. The former `readyChecks.verificationJobs`
  concept is folded into `actions.post[].job`, leaving ready checks purely
  observational.
- **API version `v1` from day one** implies a stability commitment: schema
  changes after release go through conversion webhooks, not breaking
  releases. CEL validation rules on the CRD (unique stage names, DNS-1123,
  bounds) reduce the chance of needing one.

## Reconciliation Model

### Dependency gating (`dependsOn`)

Before any work, the controller resolves `spec.dependsOn`: every referenced
StageSet must have `Ready=True` with `status.lastAppliedRevisions` populated
and `observedGeneration == generation`. If not, the run is requeued with
reason `DependencyNotReady` — identical UX to kustomize-controller, so
existing operational intuition and dashboards transfer. Cycles are detected
and reported as `Stalled`.

### Revision pinning (the atomicity property)

At the start of each run the controller:

1. Resolves every stage's `ExternalArtifact` and reads `status.artifact`
   (URL, revision, digest), requiring `Ready=True` per RFC-0012.
2. Records the set as the **run snapshot** in
   `status.lastAttemptedRevisions`.
3. Downloads and untars each artifact once into a run-scoped temp dir,
   verifying the advertised digest before anything touches the cluster.

All stages in the run build from this snapshot even if an artifact updates
mid-run; the next run picks up new revisions. This is the property chained
Kustomizations cannot offer.

Pin and fetch are one atomic step per artifact: the controller reads
`status.artifact`, immediately downloads, and digest-verifies before the
revision becomes part of the snapshot. On a 404 or digest mismatch — the
producer re-rendered and garbage-collected the old revision in the gap —
the controller re-resolves and retries within the same reconcile (bounded).
This makes StageSet robust against single-revision producers, including
stock source-controller; producer-side retention (e.g. JaaS `history` or a
GC grace period) is welcome defense in depth, never a requirement.

### Producer-aware source resolution

A stage's `sourceRef` defaults to an ExternalArtifact, but may name the
**producer** instead:

```yaml
sourceRef:
  apiVersion: jaas.metio.wtf/v1
  kind: JsonnetSnippet
  name: grafana-dashboards
```

Resolution uses RFC-0012's own linkage: every conforming producer fills
the ExternalArtifact's `spec.sourceRef` with a reference to the object it
rendered. The controller maintains a field index over that back-pointer
and resolves producer references in O(1) to the ExternalArtifact in the
same namespace whose `spec.sourceRef` matches. Properties:

- **The data plane is unchanged.** Resolution lands on an
  ExternalArtifact; fetch, digest verification, build, pinning, and the
  single-kind watch are identical for both reference styles.
- **Vendor-neutral.** No producer is special-cased and no registry flag
  exists: any controller that fills `spec.sourceRef` (JaaS, source-watcher,
  in-house renderers) gets first-class referencing for free.
- **Ambiguity fails loudly.** A producer that emits multiple artifacts
  (e.g. an ArtifactGenerator with several outputs) makes the reverse
  lookup ambiguous; resolution then fails with the candidate list in the
  status message and the user must reference the ExternalArtifact by name.
- **Error propagation is the point.** When resolution finds no Ready
  artifact, the controller fetches the producer object and surfaces *its*
  Ready condition in stage status and Events — e.g.
  `JsonnetSnippet/grafana-dashboards: Ready=False reason=ExternalVariableConflict` —
  instead of a dead-end "artifact not found". This requires only a GET at
  reconcile time, not a watch.
- **Watch surface stays minimal.** Only ExternalArtifact (and StageSet)
  are watched; a producer publishing a new revision updates its artifact,
  which triggers reconciliation. Producer failures that update nothing are
  surfaced on the retryInterval cadence; dynamic per-GVK producer watches
  are a possible later optimization, not v1 surface.
- `--no-cross-namespace-refs` applies to producer references exactly as to
  direct ones.

### Stage state machine

```text
gate    dependsOn satisfied, all ExternalArtifacts Ready, snapshot pinned

for each stage in spec.stages (in order):
  PRE      run actions.pre in list order; failure aborts the stage with the
           cluster untouched by this stage
  BUILD    kustomize build at stage path (generate kustomization.yaml if
           absent), apply stage patches, run postBuild substitution
  APPLY    server-side apply via fluxcd/pkg/ssa ResourceManager,
           field manager "stageset-controller", two-phase
           (CRDs/Namespaces first), force per stage.Force
  PRUNE    diff new stage inventory vs. stored StageInventory,
           delete objects that fell out (if stage.prune != false)
  VERIFY   kstatus poll on applied set and/or explicit checks + CEL exprs
  POST     run actions.post in list order; the stage is Ready — and the
           next stage starts — only after these succeed
  RECORD   persist StageInventory shards + stage status, emit Event

on stage failure (APPLY onward):
  run actions.onFailure (best-effort: errors evented, never retried into
  blocking), halt the run, Ready=False reason=StageFailed, requeue per
  retryInterval
```

- **No automatic rollback in the first release.** Desired state lives in
  the artifacts; a failed run halts progression and reports.
  `rollbackOnFailure` is Future Work.
- **Idempotent resume.** SSA plus re-evaluated readiness means a
  half-finished rollout resumes at the failed stage; already-Current stages
  converge to fast no-ops.

### Stage Actions (pre / post / onFailure)

Actions are the first-class replacement for Helm's hook-Job pattern. The
hook model forces users to build and maintain container images for trivial
side effects, hides the effect inside imperative pod code, exempts hook
resources from inventory, and has no failure branch. Actions instead are
small typed verbs, declared on the stage, executed by the controller:

- **`patch`** — strategic-merge or JSON6902 patch against an existing
  in-cluster object, executed under the stage's impersonated
  ServiceAccount (normal RBAC applies). Covers the bread-and-butter cases:
  flip an Ingress backend to a maintenance page, set
  `spec.maintenanceMode` on an operator CR, scale a workload.
- **`http`** — call an endpoint with method, body (inline or from Secret),
  auth headers from Secrets, expected status codes, retries, timeout. The
  Flagger-webhook-proven primitive for apps with admin APIs.
- **`wait`** — fixed duration, or block until a CEL expression over a
  referenced object holds (e.g. `status.activeSessions == 0` before
  applying).
- **`job`** — escape hatch for genuinely custom logic: Jobs rendered from
  an ExternalArtifact path, applied with a run-scoped name suffix
  (`<job>-<revision-hash>`), awaited, garbage-collected.
- **`delete`** — delete a named in-cluster object (impersonated SA,
  normal RBAC) so a subsequent apply recreates it. A missing target is
  success (idempotent). The declarative form of the manual workaround for
  effectively-immutable objects (completed Jobs, `immutable: true`
  Secrets); also the workhorse verb inside versioned migrations.
- **`apply`** — server-side-apply the manifests built from an
  ExternalArtifact path, **not** recorded in any stage inventory (so the
  inventory diff never prunes them), optionally waiting for readiness.
  This is for transient, rollout-scoped resources rather than steady-state
  stage members: because actions are gated by the per-revision ledger, the
  pair *apply at the first stage, delete at the last* stands a resource up
  only for the duration of a rollout. The canonical use is a maintenance
  page that should not consume resources between releases — `apply` the
  page Deployment + Service and `wait`, a `patch` flips the Ingress to it,
  the rollout stages run, a later `patch` flips the Ingress back, and a
  `delete` tears the page down. An `onFailure` delete guards a mid-run
  crash from orphaning it. (A *standing* maintenance pod needs no `apply` —
  `patch` actions alone flip the Ingress to an always-on page; `apply`
  exists for those who would rather not run the pod between releases.)

Semantics:

- `pre` failures abort the stage before BUILD — manifests are never
  half-applied behind a failed precondition.
- `post` actions are part of stage completion: downstream stages (and the
  Flagger gate endpoint) see `Ready` only after they succeed. This is what
  makes "maintenance off only after verified healthy" a guarantee instead
  of a convention.
- `onFailure` is the branch Helm hooks lack: per stage, declare whether a
  failed rollout keeps maintenance mode on, switches it back off, or
  notifies — best-effort, never blocking the failure report.
- **Idempotency is controller-owned.** Executed actions are recorded in
  stage status keyed by the pinned revision set, so reconcile-loop retries
  and controller restarts never re-fire side effects for a run that
  already performed them; a new revision snapshot resets the ledger.
- Actions are deliberately *not* expressible against the artifact contents
  (no templating of action definitions from the source) in `v1`; they are
  spec-level so that what can touch admin endpoints is reviewable in the
  StageSet object itself.

### Failure handling and rollback

A failed run halts at the failing stage: earlier stages are at the new
snapshot, the failed stage is applied-but-unverified, later stages remain
at the old revisions. `actions.onFailure` fires, status reports
`Ready=False reason=StageFailed` with `lastAttemptedRevisions` (new) next
to `lastAppliedRevisions` (last good run), and retries re-run the same
pinned snapshot idempotently. Three recovery layers exist:

1. **Traffic-level rollback (immediate, automatic).** Workloads under a
   Flagger Canary revert to primary on failed analysis regardless of
   StageSet; the cluster serves the old version even though the new spec
   is applied.
2. **Source revert (canonical GitOps, works in v1).** Reverting the source
   makes the producer publish a content-identical old revision; StageSet
   converges back and the inventory diff prunes whatever the bad revision
   added.
3. **`rollbackOnFailure` (opt-in).** Implemented; described below.

#### rollbackOnFailure design

We deliberately do **not** store the rendered output the way Helm stores a
release — that is exactly the in-Secret release-size ceiling we avoid. The
producer already holds every artifact, revision-addressed and immutable, in
a content store built for the job. So the default rollback stores only a
**pointer**: per stage, `{url, digest, revision}` in
`status.lastAppliedSnapshot` — a few hundred bytes, no rendered output, no
substituteFrom secret values, durable in etcd (HA-safe).

Semantics:

- Rollback restores **artifact revisions under the current spec**. On
  failure the controller re-fetches each recorded revision (digest-verified)
  and re-renders it under the live spec — current path, patches, and a
  fresh read of `postBuild` substitution — then re-applies in forward order.
  "Under the current spec" is the contract: a `substituteFrom` source that
  *also* changed in the rollback window is not reproduced (fix-forward for
  that case), but the common trigger — a bad *artifact* with stable config —
  rolls back faithfully. Pruning needs no special code: converging back to
  old content is an ordinary inventory diff.
- Failure mode: if a producer has garbage-collected the previous revision,
  rollback is terminal `PreviousRevisionUnavailable`. Best-effort by
  contract: it works exactly while producers retain. JaaS satisfies this
  (`Store.Put` keys by revision; `spec.history` controls retention) —
  snippets used with `rollbackOnFailure` should set `history: 2` or more.

**Optional external store.** For rollback that is bit-exact *and*
independent of producer retention, the controller can push each stage's
rendered output to a store it owns (keyed by artifact digest) on every
successful apply, and pull it back on rollback instead of re-fetching. This
is Helm-grade durability **without** Helm's Secret-size limit — neither
backend has a 1 MiB ceiling — and it survives producer GC. Two backends sit
behind a bytes-only `RollbackStore` seam (so an OCI backend can drop in
later), selected by mutually-exclusive flags:

- `--rollback-store-path` — a filesystem directory, typically an **RWX
  PersistentVolume** mounted into the controller. The in-cluster option,
  no object-store credentials; HA replicas share one RWX volume and leader
  election serializes writes. Access is rooted via `os.Root`, so a key can
  never traverse outside the directory.
- `--rollback-store-s3-*` — any S3-compatible bucket (minio-go), for clusters
  without RWX storage or that prefer object storage.

When the store misses (or none is configured), rollback falls back to the
pointer-and-re-fetch path above.

### Pruning semantics

Three scenarios:

1. **Object removed from a stage.** Detected by the stage-level diff during
   the run; deleted after that stage's apply, before its ready checks.
   Honors `prune: false` per stage and per-object opt-out via the
   **`stages.metio.wtf/prune: disabled`** annotation (our own annotation
   namespace; foreign controllers' annotations are ignored).
2. **Stage removed from the spec.** Orphaned StageInventories are torn down
   **in reverse recorded stage order**, after the surviving stages
   reconciled successfully.
3. **StageSet deleted.** A finalizer tears down all stage inventories in
   reverse order, then deletes the StageInventory objects themselves.
   `prune: false` stages are skipped (objects orphaned deliberately);
   orphan-on-delete is supported via the standard `DeletionPolicy` pattern.

Objects **moving between stages** in one spec change transfer ownership: the
controller computes the cross-stage ownership set before pruning, so an
object claimed by stage B's new inventory is never deleted by stage A's
diff.

## Conflict Policies for Immutable Fields

Most immutable-field failures (`immutable: true` Secrets and ConfigMaps,
Job pod templates, Service `clusterIP`/`ipFamilies`, workload selectors,
PVC storage classes) are mechanical: the user needs a per-resource answer
to "the apiserver said no", not version ceremony. The existing
`stage.force` is too blunt; it becomes sugar for the policy below.

```yaml
stages:
  - name: apps
    conflictPolicy:
      default: Fail               # current behavior; force: true == Recreate
      rules:
        - target: { kind: Secret }
          action: Recreate        # delete + re-apply on immutable conflict
        - target: { kind: ConfigMap, name: legacy-config }
          action: KeepExisting    # skip the update, warn via Event
        - target: { kind: PersistentVolumeClaim }
          action: Recreate
          allowDataLoss: true     # REQUIRED for PVC/PV; refuse otherwise
```

- Actions: `Fail` (default), `Recreate`, `KeepExisting`. The classifier
  treats both SSA immutable-field conflicts and apiserver
  `field is immutable` Invalid (422) rejections as conflicts.
- `Recreate` on PersistentVolumeClaim / PersistentVolume is refused
  unless the rule sets `allowDataLoss: true` — deleting a PVC is data
  destruction, not resource recreation, and must be said out loud.
- Per-object opt-in via the `stages.metio.wtf/force: enabled` annotation
  (mirroring kustomize-controller's per-object force) composes with the
  rules; the annotation wins for its object.
- Documentation teaches the conflict-avoiding pattern first:
  content-hash-suffixed names for immutable Secrets/ConfigMaps, where a
  change is a new object plus pruning of the old — handled natively by
  the inventory diff with no recreate gap.

## Versioned Migrations

Some transitions need semantic work a converging apply cannot express:
schema migrations, data conversions, deliberate recreation of immutable
objects. Migrations are **version-gated actions** layered on the existing
action machinery.

### Version identity

The deployed system's version must travel with the content, because
content changes flow through sources without touching the StageSet spec.

```yaml
spec:
  version:
    # exactly one of:
    fromArtifact:            # read from a stage's artifact — the default
      stage: apps            # choice; the version moves with the content
      path: .stageset/version
    # value: "2.1.0"         # spec-pinned, for fully pin-tagged setups
```

- The version file contains a single semver string. `status.version`
  records the currently deployed version, written only after a fully
  successful run. Desired != current defines a **transition**,
  regardless of whether a spec edit or a source re-render triggered it.
- `version` unset: the feature is off; behavior is exactly today's.
- `fromArtifact` configured but the file missing/unparseable: the run
  fails fast with `Stalled` reason `InvalidVersion` — a half-versioned
  system is worse than an unversioned one.

### Migration entries

```yaml
spec:
  migrations:
    - name: v2-schema-split
      to: "2.0.0"            # boundary: runs when crossing up to >= 2.0.0
      from: ">=1.2.0"        # optional constraint on current; default: any lower
      stage: apps            # anchor: runs before this stage's own pre-actions
      actions:
        - name: backup
          job: { sourceRef: { name: migration-jobs }, path: ./v2/backup }
        - name: drop-immutable-config
          delete:
            target: { apiVersion: v1, kind: Secret, name: app-config-v1, namespace: apps }
        - name: convert
          job: { sourceRef: { name: migration-jobs }, path: ./v2/convert }
```

Semantics:

- **Ladder execution.** A transition crossing multiple boundaries
  (1.0 -> 3.0) runs every matching migration in ascending `to` order.
  Equal `to` values run in list order.
- **Idempotency ledger.** Executed migrations are recorded in status
  keyed by `(name, from -> to)`: retries of a failed run never re-fire a
  completed migration, and content changes at an unchanged version run
  nothing.
- **Baselining.** Empty `status.version` with a desired version present
  records the baseline and runs no migrations (Flyway-style adoption).
- **Downgrades** are refused by default (`Stalled`,
  reason `DowngradeRequiresMigration`) — replaying upgrade migrations in
  reverse is how data dies. Explicit down-migrations are future surface.
- **Spec-resident by design.** Migration definitions live on the
  StageSet, never inside artifacts, preserving the rule that anything
  able to delete objects or call endpoints is reviewable on the object
  itself. A migration-requiring release is therefore a two-part change
  (artifact + spec) — in practice one PR. Artifact-embedded migration
  manifests were considered and rejected: a compromised source would
  gain action execution.
- **Observability.** `status.pendingMigrations` lists the migrations the
  next run will execute, so operators see the unusual work *before* it
  happens; Events fire per migration (`MigrationStarted`,
  `MigrationCompleted`, `MigrationFailed`).
- `conflictPolicy` is **implemented**: it resolves a per-object action
  (annotation `stages.metio.wtf/force: enabled` wins, then the first
  matching rule, then the effective default — `conflictPolicy.default`,
  or `Recreate` when `stage.force` is set, else `Fail`) and realizes it
  through ssa's selectors — `ForceSelector` for `Recreate`,
  `IfNotPresentSelector` for `KeepExisting`. A `Recreate` rule that
  targets a PersistentVolumeClaim / PersistentVolume is refused unless
  the rule sets `allowDataLoss: true`; the blunt `stage.force` and the
  explicit per-object annotation are treated as the operator already
  having opted in and are not gated.
- The `delete` action verb is **implemented** (idempotent delete under the
  impersonated SA), as is the `apply` verb for transient rollout-scoped
  manifests.
- `version` and `migrations` are **implemented**: the desired version comes
  from `spec.version.value` or a file in a stage's artifact
  (`fromArtifact`); a missing/unparseable version is terminal
  `InvalidVersion`; a lower desired version is terminal
  `DowngradeRequiresMigration`; first adoption baselines (records the
  version, runs nothing); an upgrade runs every migration whose target the
  transition crosses, in ascending target order, anchored before its
  stage's pre-actions, gated by an in-flight ledger so a retry skips
  finished ones; `status.version` advances only after a fully successful
  run; `MigrationStarted`/`Completed`/`Failed` Events fire per migration.
- `rollbackOnFailure` is **implemented**: a successful run records only a
  per-stage *pointer* (`{url, digest, revision}`) in
  `status.lastAppliedSnapshot` — never the rendered output, so there is no
  Helm-style release-size limit and no substituteFrom secret values in
  status. On a stage failure the controller re-fetches those revisions
  (digest-verified) and re-renders them under the live spec, re-applying in
  forward order. A producer-GC'd revision is terminal
  `PreviousRevisionUnavailable` (best-effort while producers retain). An
  optional external store — a filesystem/RWX PVC (`--rollback-store-path`)
  or S3 (`--rollback-store-s3-*`) — holds the bit-exact rendered output for
  rollback independent of producer retention. See the rollbackOnFailure
  design section.

No StageSet field shape is reserved any longer — every shape the CRD carries
is implemented.

## Inventory Storage: Sharded `StageInventory` CRD

### Requirements

Inventory must (a) survive controller restarts and reinstalls, (b) support
concurrent-safe updates, (c) be inspectable with standard tooling, and
(d) scale to very large stages without the single-object ceiling that makes
Helm's release storage (entire gzipped manifests in one Secret) a known
pain point.

### Design

A dedicated namespaced CRD, `StageInventory` (`stages.metio.wtf/v1`),
holding only object identifiers — never manifests:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageInventory
metadata:
  name: platform-operators-00            # <stageset>-<stage>-<shard>
  namespace: flux-system
  labels:
    stages.metio.wtf/stage-set: platform
    stages.metio.wtf/stage: operators
    stages.metio.wtf/shard: "0"
  ownerReferences: [ …StageSet… ]        # GC safety net
spec:
  stagePosition: 1                       # recorded order for reverse teardown
  entries:
    - id: cert-manager_cert-manager_apps_Deployment
      v: v1
    - id: _letsencrypt_cert-manager.io_ClusterIssuer
      v: v1
```

- An entry is ~100–150 bytes. A single shard stays well under the ~1.5 MiB
  etcd object ceiling at a conservative cap of **5,000 entries**; the
  controller opens shard `-01`, `-02`, … beyond that. Capacity is therefore
  unbounded in practice — a 100k-object stage is 20 small objects.
- Shards are written before stage status is updated (write-ahead), so a
  crash between apply and record is recovered by re-diffing against the
  union of shards — pruning never acts on a partially written inventory.
- `ownerReferences` to the StageSet make Kubernetes GC the safety net, but
  ordered teardown is driven by the finalizer (GC alone would not respect
  reverse order or `prune: false`).
- Inspectability: `kubectl get stageinventories -l stages.metio.wtf/stage-set=platform`
  shows exactly what each stage owns; velero/backup tooling captures it for
  free; RBAC on the CRD controls who can read deployment topology.

### ApplySet compliance (KEP-3659)

Each stage is additionally a spec-compliant **ApplySet**, with the
StageInventory shard `-00` acting as the parent object:

- The StageInventory CRD carries the
  `applyset.kubernetes.io/is-parent-type: "true"` label, making it a valid
  custom-resource parent per the spec.
- The parent carries the `applyset.kubernetes.io/id` label (derived from
  name/namespace/kind/group per the spec's hashing scheme) plus the
  bounded hint annotations (`tooling`, contains-group-kinds,
  additional-namespaces). Hints grow with the number of *kinds and
  namespaces*, never with object count.
- Every object the stage applies receives the
  `applyset.kubernetes.io/part-of: <id>` member label as part of the SSA
  payload.
- Hint updates follow the spec's crash-safety protocol: superset before
  apply, shrink after prune.

What this buys:

(To be explicit about layering: the ApplySet parent role is *additional*
metadata on shard `-00` — StageInventory remains the deep, authoritative
entry store. It is not reduced to a thin hint-only parent.)

- **On-object accountability.** Ownership is visible on the resource
  itself: `kubectl get all -l applyset.kubernetes.io/part-of=<id>` answers
  "what does stage X own" with zero project-specific tooling, and any
  future ApplySet-aware ecosystem tool understands our groupings.
- **Drift detection.** Implemented inline on the apply path rather than as
  a separate label-cross-check reconciler: server-side apply already
  re-asserts desired state every reconcile, so when a *steady-state*
  reconcile (no new artifact revision) reports `created`/`configured`
  changeset entries, the live object was mutated or deleted out-of-band and
  the apply corrected it. Those entries raise a `DriftCorrected` Event and
  increment `stageset_drift_corrected_total`. This reuses the existing apply
  and the interval reconcile — no extra LIST-every-group-kind pass — and
  catches both out-of-band edits and deletions. A label-discovery pass
  (for stripped membership labels) remains possible future work.
- **Remote clusters without CRD installs.** The spec permits
  ConfigMap/Secret parents, and requires the parent to live in the target
  cluster; for `kubeConfig` applies the controller uses ConfigMap parents
  remotely, keeping the StageInventory CRD an in-cluster-only dependency.

The **authoritative prune source remains the explicit entries**, not label
discovery. Label-based pruning fails open on label stripping (silently
orphaning objects with no record they existed) and relies on per-group-kind
LIST calls whose server-side cost scales with cluster size rather than
stage size. Entry-based diffing has neither problem. A future
`inventoryMode: applyset` could offer entry-free operation (unbounded by
construction, no shards) for users who accept discovery semantics — the
labels and parents required for it are already in place, so it would be a
non-breaking addition.

The ApplySet KEP is alpha in kubectl (merged since 1.27 behind
`KUBECTL_APPLYSET=true`, beta under consideration). This is acceptable
risk: we depend on the versioned on-object *specification* (the `-v1` id
suffix exists precisely to allow evolution), not on kubectl's CLI feature
gate, and the spec is explicitly published for third-party tooling
adoption.

### Inventory modes and migration

`--inventory-mode` selects the strategy globally:

- **`entries`** — explicit entries only; no ApplySet metadata is written.
  For environments that reject alpha-spec labels on their objects or have
  conflicting label tooling.
- **`hybrid`** (default) — entries authoritative, ApplySet labels and
  parents continuously maintained.
- **`applyset`** — discovery-based: no entries persisted; pruning lists
  hinted group-kinds with the member-label selector. Entry-free and
  unbounded by construction; fails open on label stripping.

Each StageSet records the mode its stored inventory currently satisfies in
`status.inventoryMode`. On a flag change the controller migrates objects
individually on their next successful reconcile (suspended objects on
resume), so a flag flip never stampedes the cluster:

- `entries → hybrid`: label every inventoried object via SSA, create
  parent metadata. Lossless.
- `hybrid → applyset`: labels were continuously maintained; entry shards
  are cross-checked against discovery once, then dropped. Lossless.
- `entries → applyset`: automatically transits through one hybrid-style
  labeling reconcile before entries are dropped (discovery cannot see
  unlabeled objects).
- `applyset → hybrid/entries`: entries are **reconstructed from
  discovery** and therefore contain exactly what label-listing can see.
  Objects that lost their member label while in `applyset` mode are not
  recovered — but they were already invisible to pruning in that mode, so
  the migration loses nothing the mode had not already lost.

Mode migrations never delete cluster objects themselves; only the
ordinary prune diff does, and it runs against the post-migration
inventory.

### Alternatives considered for storage

1. **PVC attached to the controller** (unlimited size, e.g. an embedded
   bbolt/sqlite file). Rejected: turns the controller into a StatefulSet
   with RWO volume-attachment coupling to node and AZ, complicates
   leader-election failover and HA, makes inventory invisible to kubectl /
   RBAC / audit / backup tooling, loses optimistic-concurrency semantics,
   and adds a StorageClass prerequisite to a GitOps controller. The size
   problem it solves is already solved by sharding, because we store IDs,
   not manifests — Helm's mistake was the latter, not using etcd per se.
2. **Inventory inside StageSet `.status`** (kustomize-controller's model).
   Rejected: couples total inventory size across *all* stages to one
   object's ~1.5 MiB limit with a wedged-reconciler failure mode at the
   ceiling; ships the full entry list to every StageSet watcher on every
   status patch (notification-controller, UIs, `kubectl -w`), which is
   costly for a controller with frequent per-stage phase transitions; and
   loses prune history on disaster recovery, since backup tooling such as
   Velero does not restore status subresources by default — StageInventory
   keeps entries in `spec` deliberately so restores preserve them.
   Entry-free status-resident inventory (hints only, discovery-based
   pruning) was also considered once ApplySet labels existed; rejected for
   the reasons in alternative 4.
3. **ConfigMap shards.** Workable and dependency-free, but a typed CRD gives
   schema validation, printer columns, and clean RBAC separation for ~50
   lines of API types. ConfigMaps remain the fallback if CRD count is a
   concern, and are used as ApplySet parents on remote clusters regardless.
4. **Pure ApplySet (label-discovery) inventory.** Store no entries; prune
   by listing hinted group-kinds with the member-label selector (the
   kubectl `--applyset` model). Unbounded by construction and maximally
   interoperable, but pruning fails open when labels are stripped and its
   LIST cost scales with cluster size. Adopted as the *labeling layer* and
   a possible future opt-in mode, rejected as the authoritative source —
   see ApplySet compliance above.

## Controller Architecture

- **Scaffolding:** kubebuilder + `fluxcd/pkg/runtime` (conditions, patch
  helper, events, leader election, reconcile annotations), consistent with
  GitOps Toolkit controllers.
- **Source watching:** a single watch on `ExternalArtifact` with an index
  from artifact → StageSets referencing it; new revisions trigger
  reconciliation immediately. A second watch on StageSet feeds `dependsOn`
  wake-ups (dependents are requeued when a dependency becomes Ready).
- **Build:** `fluxcd/pkg/kustomize` (secure build with load restrictions,
  generator, post-build substitution) — the same code paths
  kustomize-controller uses.
- **Apply/wait/prune:** `fluxcd/pkg/ssa` `ResourceManager`
  (ApplyAllStaged, WaitForSet, DeleteAll): SSA conflict handling,
  immutable-field force-recreate, kstatus waiting.
- **CEL checks:** `fluxcd/pkg/runtime/cel` status readers, wired into
  WaitForSet — expression-compatible with `healthCheckExprs`.
- **Multi-tenancy:** impersonation through `serviceAccountName`, remote
  apply via `kubeConfig`, `--no-cross-namespace-refs` gating cross-namespace
  `sourceRef` and `dependsOn` — parity with kustomize-controller so
  platform policies transfer unchanged. The two compose: a run can target a
  remote cluster *and* impersonate an SA there. `kubeConfig.secretRef` (a
  self-contained kubeconfig in a Secret) is supported; the cloud-provider
  `kubeConfig.configMapRef` path is rejected until provider auth is wired.
  Apply/prune/verify/actions use the target connection; StageInventory and
  status stay on the controller cluster (the inventory CRD is never a remote
  dependency).

## Status

```yaml
status:
  conditions:
    - type: Ready          # all stages applied + verified at lastAppliedRevisions
    - type: Reconciling
    - type: Stalled        # terminal failure until spec/artifact changes
  observedGeneration: 4
  lastAttemptedRevisions:
    flux-system/platform-crds:      v1.4.0@sha256:1a2b…
    flux-system/operators-bundle:   v2.0.3@sha256:9f8e…
  lastAppliedRevisions: { …same shape, last fully successful run… }
  stages:
    - name: crds
      phase: Ready          # Pending | Applying | Pruning | Verifying | Ready | Failed
      appliedRevision: v1.4.0@sha256:1a2b…
      entriesCount: 14
      shards: 1
    - name: operators
      phase: Verifying
      message: "waiting for ClusterIssuer/letsencrypt to become Ready"
```

Events and notification-controller alerts fire per stage transition
(`StageApplied`, `StageReady`, `StageFailed`, `StagePruned`), so alerts say
*which* stage is stuck rather than reporting a generic reconcile failure.

## Progressive Delivery: Flagger Integration

StageSet does not implement traffic shifting, metric analysis, or
blue/green switching — that boundary mirrors the Helm decision: StageSet
owns *when and in what order*, Flagger owns *how traffic moves*. The
integration has three layers.

### Canary-gated stages (no new code)

A Flagger `Canary` is an ordinary manifest inside the stage's artifact.
The stage gates on it via CEL:

```yaml
stages:
  - name: frontend
    sourceRef:
      name: frontend-bundle        # Deployment + Service + Canary CR
    readyChecks:
      timeout: 30m                 # must exceed the Canary analysis duration
      exprs:
        - apiVersion: flagger.app/v1beta1
          kind: Canary
          current: status.phase == 'Initialized' || status.phase == 'Succeeded'
          inProgress: status.phase in ['Progressing', 'WaitingPromotion', 'Promoting', 'Finalising']
          failed: status.phase == 'Failed'
```

Resulting semantics:

- A new artifact revision changes the pod spec → Flagger starts analysis →
  the stage stays in `Verifying` until promotion completes → downstream
  stages move only after the switch actually succeeded.
- On failed analysis, Flagger reverts traffic to primary and sets
  `Failed`; the stage fails and the StageSet halts with the cluster still
  healthy (primary serves the previous version). Flagger re-analyzes only
  on the next spec change, matching StageSet's stall-until-revision-change
  behavior.
- The default kstatus wait is meaningless for canary targets (Flagger
  scales the original Deployment to zero, which kstatus reports as
  Current); the Canary expression is the authoritative gate. Documentation
  must call this out.
- Pruning interaction: removing a Canary from an artifact deletes the CR;
  users should set `revertOnDeletion: true` on Canaries so Flagger restores
  the original Deployment/Service before its finalizer releases the object.

### Easy-mode blue/green for teams

Flagger's `provider: kubernetes` performs blue/green without any service
mesh or ingress controller: it tests the new version through a preview
Service via webhooks, then flips the selector. The team-facing recipe is
therefore artifact-only — no infrastructure prerequisites beyond Flagger
itself:

1. A shared **Kustomize component** (shipped with this project, consumed
   via the stage artifact) that generates, per Deployment, a Canary CR with
   `provider: kubernetes`, sensible `iterations`/`interval`, and a
   conformance-test webhook stub.
2. The CEL gate above, shipped as a documented, copy-pasteable snippet
   alongside HelmRelease and Certificate expressions (deliberately not API
   surface — see Resolved Design Decisions).

### Stage gate webhook (new feature)

Flagger analysis supports `confirm-rollout` and `confirm-promotion`
webhooks that block until an endpoint returns HTTP 200. stageset-controller
exposes a read-only gate endpoint:

```text
GET /gate/{namespace}/{stageset}/{stage}
200  → stage phase is Ready at the currently pinned revision set
403  → otherwise (body carries phase + message for debugging)
```

This makes coordination bidirectional: StageSet waits on Flagger
(Canary-gated stages), and Flagger waits on StageSet. The canonical use
case is the migration-before-promotion problem:

```yaml
# stage "migrations" runs schema migration Jobs; the app's Canary refuses
# to promote until that stage is green for the same rollout:
analysis:
  webhooks:
    - name: migrations-complete
      type: confirm-promotion
      url: http://stageset-controller.flux-system/gate/flux-system/platform/migrations
```

No annotations, manual approvals, or in-pod schema checks — the ordering
guarantee is declared once and enforced by the two controllers. The
endpoint is read-only, unauthenticated-safe by content (it leaks only a
stage phase), and optionally gated by NetworkPolicy; it is served from the
controller's existing HTTP mux, an estimated ~200 LOC.

The gate is deliberately a plain state gate, matching how Flagger webhooks
are conventionally used (static URL, 200/non-200). Within a single
StageSet it is belt-and-suspenders: stage ordering already guarantees the
migration stage is Ready for the pinned snapshot before the canary's
pod-spec change is even applied. Its real purpose is gating across
StageSets (e.g. a platform team's pipeline gating an app team's Canary);
in that cross-object case a small race window exists during rapid
successive rollouts, which is documented rather than solved with
revision-pinned URLs.

## Alternatives Considered (whole-system)

1. **Extend `Kustomization` with a `stages` field.** Rejected: complicates
   the most-used Flux API for a minority use case; the single-inventory
   reconciler model is structurally incompatible with per-stage pruning.
2. **An orchestrator CR generating chained Kustomizations.** Simpler to
   build, but cannot deliver revision pinning, leaks implementation objects
   into user namespaces, and aggregate status/teardown ordering stay
   awkward.
3. **Multi-source-kind stages (design v1).** Superseded by the
   ExternalArtifact-only pivot; composition belongs to ArtifactGenerator and
   RFC-0012 producers.
4. **Argo CD sync waves.** Credited prior art proving demand for ordered
   apply — but waves are resource annotations (afterthought-shaped), with
   no per-wave pruning or revision pinning.

## Security Considerations

- Artifact digests are verified after download (RFC-0012 contract); a
  mismatch fails the run before the cluster is touched.
- Kustomize build runs with load restrictions (no file access outside the
  artifact root).
- Every cluster write of a run — the SSA apply, health-check reads, the
  inventory-diff prune, finalizer teardown, and the typed actions (Job
  creation, `patch`, secret reads) — runs under `spec.serviceAccountName`
  when set: a clone of the manager's rest config with `Impersonate-User`
  set to `system:serviceaccount:<namespace>:<name>` (kustomize-controller
  parity, header impersonation — the controller's own ServiceAccount needs
  only the `impersonate` verb on `serviceaccounts`, never the union of
  tenant permissions). A run therefore reaches no further than the named
  SA's RBAC; the apiserver enforces the ceiling. When unset, writes use the
  controller's own client (the single-tenant default). Bookkeeping the
  tenant must not see — StageInventory shards, StageSet status — stays on
  the controller client. The per-`<namespace>/<sa>` client is cached so the
  RESTMapper and transport are built once, not per reconcile.
- `http` actions are a server-side request forgery (SSRF) vector: the
  controller occupies a privileged network position (in-cluster, typically
  able to reach un-Ingressed Services, the apiserver, and cloud instance
  metadata endpoints such as 169.254.169.254), and it issues requests
  authored by anyone permitted to create StageSets. Mitigations:
  `--allowed-action-hosts` (glob list) is evaluated before each request
  **and after every redirect**; connections pin the resolved IP to defeat
  DNS rebinding; loopback, link-local, and other special-purpose ranges
  are always denied unless explicitly listed. Single-tenant default is
  allow-all minus the always-denied ranges; multi-tenant installations
  should configure the allowlist alongside `--no-cross-namespace-refs`.
- Cross-namespace `sourceRef`/`dependsOn` are disabled cluster-wide via
  flag for multi-tenant installations.
- StageInventory objects reveal deployment topology; RBAC on the CRD should
  match StageSet read access.

## Resolved Design Decisions

The following were open questions in earlier drafts; decisions are
recorded here with rationale.

1. **Inventory shard cap** — 5,000 entries per shard, overridable via the
   `--inventory-shard-cap` controller flag.
2. **`dependsOn` semantics** — exactly kustomize-controller's
   (`Ready=True` + observed generation), no extensions. Stage-granular
   gating (`dependsOn[].stage`) remains Future Work.
3. **Helm-hook annotations in artifacts** — applied as ordinary resources
   with a warning Event by default. A per-stage `applyHelmHookResources`
   toggle (default `true`) lets users instead strip
   `helm.sh/hook`-annotated resources during BUILD; stripped resources are
   reported via Event.
4. **Inventory storage** — the StageInventory CRD stays, doubling as the
   ApplySet parent type.
5. **Ready-check presets** — not API surface. Common CEL expressions
   (Flagger Canary, HelmRelease, cert-manager Certificate) ship as
   documented, copy-pasteable snippets, avoiding third-party API shapes in
   our release cycle.
6. **Gate endpoint revision pinning** — dropped. Flagger webhook
   integrations are conventionally plain state gates (static URL,
   200/non-200, as with the loadtester's manual-gating endpoints), and
   within a single StageSet stage ordering already guarantees
   migration-before-promotion for the same snapshot, because the canary's
   pod-spec change is itself applied by a later stage of that snapshot.
   The plain gate serves the cross-StageSet case; the residual race during
   rapid successive cross-StageSet rollouts is documented, not solved in
   the API.
7. **Auto-revert for actions** — skipped permanently. Reversibility cannot
   be provided uniformly: `http` and `job` actions are arbitrary side
   effects with no general inverse, and patches are invertible only for a
   subset of operations. A capability that exists for some action types
   and not others invites incorrect mental models; explicit symmetric
   pre/post actions remain the contract.
8. **HTTP action egress policy** — `--allowed-action-hosts` implemented
   with always-denied special-purpose ranges, redirect re-validation, and
   DNS pinning; see Security Considerations for the SSRF threat model.
9. **Inventory mode** — a global
   `--inventory-mode=entries|hybrid|applyset` controller flag (default
   `hybrid`) rather than a per-object field, because inventory semantics
   are a cluster operator's correctness/cost tradeoff that tenants should
   not opt out of. See Inventory Modes and Migration.
10. **Producer-aware source references** — `sourceRef` accepts optional
    `kind`/`apiVersion` (default ExternalArtifact), resolved via the
    artifact's RFC-0012 `spec.sourceRef` back-pointer; see Producer-aware
    source resolution. Producer retention minimums (e.g. requiring JaaS
    `history >= 2`) were rejected: the pin-fetch race is handled
    atomically in the consumer, where it protects against *all*
    single-revision producers including stock source-controller, rather
    than encoding one consumer's needs into one producer's API.

## Future Work

- DAG execution between stages (parallel branches with join points).
- `dependsOn[].stage` for fine-grained cross-StageSet gating.
- `rollbackOnFailure` per the designed semantics in Failure handling and
  rollback (re-fetch pinned revisions + snapshotted substitution inputs).
- Drift-detection interval decoupled from artifact-change reconciliation.
- A CLI plugin (`flux`-style trace/status) for readable rollout progress.
- A maintained Kustomize component library (blue/green, canary) for
  team-facing progressive-delivery boilerplate.
- Dynamic per-GVK watches on producer objects for faster failure surfacing.
- Explicit down-migrations (`direction: Down`) for declared, reviewed
  downgrade paths.
- A shared SSRF-guard module extracted with JaaS's `internal/urlguard`, so
  the redirect-revalidation and DNS-pinning logic exists once.
