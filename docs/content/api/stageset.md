---
title: StageSet
description: Every field of the StageSet resource — the one you author.
tags: [api, stages, crd, sources]
---

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
```

A `StageSet` is a namespaced [Kubernetes](https://kubernetes.io/docs/) resource
describing an ordered set of stages. Only `spec.stages` is required; everything else
refines scheduling, security, gating, versioning, and rollback. Every field below is
shown in YAML at least once.

The smallest valid StageSet:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: my-app
  namespace: default
spec:
  stages:
    - name: app
      sourceRef:
        name: my-app
```

---

## Scheduling

```yaml
spec:
  interval: 5m                  # optional: reconcile cadence (default: --default-interval)
  retryInterval: 1m             # cadence after a failed run (default: interval)
  driftDetectionInterval: 2m    # faster drift correction than interval (optional)
  timeout: 5m                   # default per-stage timeout (optional)
  suspend: false                # pause reconciliation without deleting (default false)
```

- **`interval`** (optional) — steady-state reconcile cadence; each reconcile
  re-resolves sources, re-asserts desired state (correcting drift), and prunes.
  **When omitted, the controller's `--default-interval` is used** (the chart's
  `controller.defaultInterval`, default `10m`), so most StageSets can leave it out.
- **`retryInterval`** — retry cadence after a failure; falls back to `interval`.
- **`driftDetectionInterval`** — a shorter cadence dedicated to healing out-of-band
  drift when you need it tighter than `interval`.
- **`timeout`** — how long any one stage may take before it fails; override per
  stage with `stages[].timeout`.
- **`suspend`** — short-circuits to `Ready=False / Suspended`, leaving applied state
  running. Use [`stagesetctl reconcile --force`](/cli/reconcile/) to run once while
  suspended. See the [`Suspended` runbook](/runbooks/suspended/).

## Ordering between StageSets

`dependsOn` gates this StageSet on others being Ready at their observed generation
— cross-release ordering. (Ordering *within* a StageSet is the order of `stages`.)

```yaml
spec:
  dependsOn:
    - name: platform
      namespace: platform-system
```

## Security and targeting

```yaml
spec:
  serviceAccountName: payments-deployer   # impersonated for every cluster operation
  kubeConfig:
    secretRef:
      name: prod-eu-kubeconfig            # apply to a remote cluster
  decryption:
    provider: sops                        # decrypt SOPS files in stage sources
    secretRef:
      name: sops-age                      # holds an age key under *.agekey
```

- **`serviceAccountName`** — the ServiceAccount the controller applies as (via a
  minted TokenRequest token on the local cluster); the StageSet can do exactly what
  its RBAC allows. A stage can override it with its own
  [`serviceAccountName`](#serviceaccountname) to target a different tenant. See
  [multi-cluster and tenancy](/security/multi-cluster/).
- **`kubeConfig`** — apply to a remote cluster. Set exactly one of:
  - **`secretRef`** — a Secret holding a self-contained kubeconfig (with its own
    embedded credentials).
  - **`configMapRef`** — a ConfigMap selecting cloud-provider workload-identity
    auth (`aws`, `azure`, `gcp`, or `generic`); the cluster bearer token is
    minted by the cloud's IAM/STS. See
    [multi-cluster and tenancy](/security/multi-cluster/).
- **`decryption`** — decrypt SOPS-encrypted files (`age`) in every stage's source
  before they are built. `provider` is `sops`; `secretRef` names the key Secret,
  read under `serviceAccountName`. See [secrets encryption](/security/encryption/).

## Versioning and migrations

Versioning is off unless `spec.version` is set. Set **exactly one** of
`value` / `fromObject` / `fromArtifact`:

```yaml
spec:
  version:
    # A StageSet has ONE version that all its stages converge on. fromObject reads
    # it from a rendered object — by default the app.kubernetes.io/version label,
    # so it travels in the manifests (works for every source kind, including
    # JaaS). The recommended default.
    fromObject:
      # stage: app                     # optional; which stage's output to read the version from — defaults to the first stage (the leading stage carries the new version first)
      kind: Deployment
      name: web
      # apiVersion: apps/v1            # optional; narrows an ambiguous Kind+Name
      # fieldPath: "{.data.version}"   # optional JSONPath; defaults to the version label
    # value: "2.1.0"                   # …or pin it inline
    # fromArtifact: { stage: app, path: VERSION }   # …or read a VERSION file (Git/OCI/Bucket)

  migrations:
    - name: backfill-ledger-2-0 # idempotency-ledger / Events name
      from: ">=1.0.0, <2.0.0"   # optional: a semver CONSTRAINT on the current version
      to:   "2.0.0"             # required: the EXACT boundary this migration crosses
      stage: app                # runs before this stage's pre-actions
      actions:                  # the same Action shape used by stages (see below)
        - name: backfill
          job:
            sourceRef:
              name: ledger-backfill-job
```

`to` is an exact version (like `version.value`). `from` is a semver
**constraint**, not an exact version — it accepts ranges such as
`>=1.0.0, <2.0.0`, `1.x`, or `^1.2`, matched against the currently deployed
version. The migration fires only when the deployed version satisfies `from`
*and* the run crosses up to `to`. See
[versioned migrations](/gating/versioned-migrations/).

## Rollback

```yaml
spec:
  rollbackOnFailure: true       # restore last-good revisions on a failed run
  onRollback:                   # best-effort cleanup after the restore
    - name: disable-maintenance-mode
      job:
        sourceRef:
          name: moodle-maintenance-off
```

Needs a rollback store configured; see [rollback](/gating/rollback/).

- **`onRollback`** — StageSet-level actions run best-effort **after** a rollback
  restores the previous manifests (both `rollbackOnFailure` and a promotion gate's
  `onFailure: Rollback` revert). They run against the restored state under
  [`serviceAccountName`](#serviceaccountname), are **not** gated by the
  per-revision action ledger (so they fire on every rollback), and take the same
  operation types as stage [actions](#actions). Names must be unique within the
  list. Use it for cleanup that only makes sense once the old version is back —
  e.g. lifting an application maintenance mode a failed upgrade enabled. See
  [post-rollback cleanup](/defining-a-release/actions/#post-rollback-cleanup-onrollback).

## Update windows

Gate *when* new revisions roll out. Each window is `Allow` or `Deny`, recurring
(cron) **or** absolute (from/to). `windowScope` controls how strict a closed window
is.

```yaml
spec:
  windowScope: Updates          # Updates (default): hold rollouts, keep correcting
                                # drift. All: a hard freeze — no applies at all.
  updateWindows:
    - type: Deny                # Deny always wins over Allow
      schedule: "0 9 * * MON-FRI"   # 5-field cron: window start
      duration: 8h
      timeZone: Europe/Berlin   # IANA tz (default UTC)
    - type: Deny                # an absolute one-off freeze
      from: 2026-12-24T00:00:00Z
      to:   2026-12-27T00:00:00Z
```

A recurring window uses `schedule` + `duration`; an absolute window uses
`from` + `to`. See [update windows](/gating/update-windows/).

---

## Stages

`stages` (required, min 1) is the ordered list. A stage with every field set:

```yaml
spec:
  stages:
    - name: app                 # required; DNS-label, unique in the StageSet
      sourceRef:
        name: my-app            # required
        kind: ExternalArtifact  # default; also GitRepository/OCIRepository/Bucket
                                # directly, or a producer (e.g. JsonnetSnippet)
        apiVersion: source.toolkit.fluxcd.io/v1   # required for a producer kind
        namespace: other-ns     # default: the StageSet's namespace
      path: ./overlays/prod     # path inside the artifact (default ./)
      serviceAccountName: payments-eu-deployer  # per-stage identity (default: spec.serviceAccountName)
      prune: true               # GC objects that leave the stage (default true)
      timeout: 3m               # per-stage timeout (default: spec.timeout)
      force: false              # sugar for conflictPolicy.default: Recreate
      applyHelmHookResources: true  # apply helm.sh/hook objects as ordinary ones
      patches: []               # Kustomize patches applied after build
      conflictPolicy: {}        # see below
      postBuild: {}             # see below
      actions: {}               # see below
      readyChecks: {}           # see below
```

`sourceRef.kind` defaults to `ExternalArtifact`, so the common case is just
`sourceRef: { name: … }`. A `sourceRef` resolves to a [Flux](https://fluxcd.io/)
artifact in one of three ways: an `ExternalArtifact`
([RFC-0012](https://github.com/fluxcd/flux2/tree/main/rfcs), the default), a classic
Flux source — `GitRepository`, `OCIRepository`, or `Bucket` — consumed **directly**,
or any other kind treated as a *producer* and resolved to its `ExternalArtifact` via
the back-pointer index. See
[stages and sources](/defining-a-release/stages-and-sources/#source-kinds) and
[producer-aware sources](/integrations/producer-aware-sources/).

`sourceRef.kind` is intentionally **open**: it defaults to `ExternalArtifact` but
accepts any producer kind, because a producer reference is resolved through the
`ExternalArtifact`'s RFC-0012 `spec.sourceRef` back-pointer rather than matched
against a fixed set. This is a deliberate divergence from a source consumer that
restricts its source kinds to a closed enum — here the resolution is
producer-aware, so a new producer kind works without a schema change.

### serviceAccountName

The ServiceAccount this stage's cluster operations run as — apply, prune,
readiness verification, and [actions](#actions) — overriding the StageSet-level
`spec.serviceAccountName`. Empty inherits the StageSet default. Each stage's
writes are bounded by its own ServiceAccount's RBAC, so a single StageSet can
roll a change through environments that live under different identities:

```yaml
spec:
  serviceAccountName: staging-deployer      # default for stages that omit it
  stages:
    - name: staging
      sourceRef: { name: my-app }           # runs as staging-deployer
    - name: production
      serviceAccountName: prod-deployer      # runs as prod-deployer instead
      sourceRef: { name: my-app }
```

The same identity that applies a stage also prunes and tears it down, so a
per-stage ServiceAccount with create rights can garbage-collect its own objects.
On the local cluster the identity is assumed by minting a short-lived TokenRequest
token; a remote [`kubeConfig`](#security-and-targeting) instead impersonates the
ServiceAccount against that cluster's credentials. SOPS decryption keys are read
under the StageSet-level identity, since `spec.decryption` is a StageSet field.
See [multi-cluster and tenancy](/security/multi-cluster/).

### patches

[kustomize](https://kubectl.docs.kubernetes.io/) strategic-merge or JSON6902
patches, applied after the build:

```yaml
      patches:
        - patch: |
            - op: replace
              path: /spec/replicas
              value: 6
          target:
            kind: Deployment
            name: web
```

### postBuild

Variable substitution after build and patching:

```yaml
      postBuild:
        substitute:
          cluster_name: prod-eu        # inline key/value
        substituteFrom:
          - kind: ConfigMap            # required: ConfigMap or Secret
            name: cluster-vars
          - kind: Secret
            name: cluster-secrets
            optional: true             # tolerate a missing source
```

### conflictPolicy

Per-resource answers to apply conflicts (immutable fields, ownership):

```yaml
      conflictPolicy:
        default: Fail                  # Fail (default) | Recreate | KeepExisting
        rules:
          - target:                    # partial selector; unset fields match all
              apiVersion: batch/v1
              kind: Job
            action: Recreate
          - target:
              kind: PersistentVolumeClaim
              name: scratch
            action: Recreate
            allowDataLoss: true        # required to Recreate a PVC/PV
```

See [conflict policies](/defining-a-release/conflict-policies/).

### readyChecks

Gate when the stage counts as complete:

```yaml
      readyChecks:
        timeout: 5m
        disableWait: false             # true = apply without waiting for readiness
        checks:                        # explicit objects, evaluated with kstatus
          - apiVersion: apiextensions.k8s.io/v1
            kind: CustomResourceDefinition
            name: ledgers.example
        exprs:                         # custom health via CEL expressions (healthCheckExprs shape)
          - apiVersion: db.example/v1
            kind: Database
            current: "status.phase == 'Running'"
            inProgress: "status.phase in ['Pending','Provisioning']"
            failed: "status.phase == 'Failed'"
```

Health expressions use [CEL](https://github.com/google/cel-spec). See
[ready checks](/defining-a-release/ready-checks/).

---

## Actions

`stages[].actions` (and `migrations[].actions`) carry typed steps. Each `Action`
has a `name`, optional `timeout`/`retries`, an optional `scope` (pre/post only),
and **exactly one** operation block.

`scope` selects how often a pre/post action runs: `Revision` (default) once per
pinned artifact revision, `Version` once per resolved `spec.version` episode — so
revision churn at a fixed version stops re-running upgrade choreography — or
`Lifetime` once ever, for a durable bootstrap. `Version` requires `spec.version`;
`Lifetime` records its completion in a `StageLedger`. See
[action scope](/defining-a-release/actions/#scope-revision-version-or-lifetime).

```yaml
      actions:
        pre:        # before apply; failure aborts the stage with nothing applied
          - name: db-migrate
            timeout: 10m
            retries: 2
            job:
              sourceRef: { name: my-app-migrations }
              path: ./jobs
        post:       # after verify; the stage is Ready only if these pass
          - name: smoke-test
            http:
              url: https://my-app.internal/healthz
              method: GET                    # default POST
              expectedStatus: [200]          # default: any 2xx
              headersFrom:
                - name: gate-token
                  key: token
        onFailure:  # best-effort on any failure from apply onward
          - name: page-oncall
            http:
              url: https://alerts.internal/stageset-failed
```

The six operation types — one per Action:

```yaml
# patch — patch an existing object
- name: enable-traffic
  patch:
    target: { apiVersion: v1, kind: Service, name: web }
    type: merge                # merge (default) | json6902
    patch: '{ "spec": { "selector": { "release": "green" } } }'

# http — call an endpoint (hosts gated by --allowed-action-hosts)
- name: approve
  http:
    url: https://gate.internal/approve
    bodyFrom: { name: approve-secret, key: body }

# wait — block for a duration or until a CEL expr holds
- name: settle
  wait:
    duration: 30s
- name: until-available
  wait:
    target: { apiVersion: apps/v1, kind: Deployment, name: web }
    expr: "status.availableReplicas >= 3"
    timeout: 5m

# job — render and await Jobs from an artifact
- name: migrate
  job:
    sourceRef: { name: my-app-migrations }
    path: ./jobs

# delete — remove an existing object (missing = success)
- name: drop-legacy
  delete:
    target: { apiVersion: batch/v1, kind: Job, name: legacy-migration }

# apply — transient, rollout-scoped manifests (NOT inventory-tracked, never pruned)
- name: canary
  apply:
    sourceRef: { name: my-app-canary }
    path: ./
    wait: true                 # block until applied objects report Ready
```

See [actions](/defining-a-release/actions/).

---

## status

`status` is controller-owned and read-only. A representative snapshot:

```yaml
status:
  observedGeneration: 7
  conditions:
    - type: Ready
      status: "True"
      reason: Succeeded
      message: All 2 stages applied
  lastHandledReconcileAt: "2026-06-15T09:21:04Z"
  lastAttemptedRevisions: { payments/payments-app: sha256:1a2b }
  lastAppliedRevisions:   { payments/payments-app: sha256:1a2b }
  version: "2.1.0"
  pendingMigrations: []
  executedMigrations: []
  stages:
    - name: infrastructure
      phase: Ready             # Pending|Applying|Pruning|Verifying|Ready|Failed
      appliedRevision: sha256:9f3c
      entriesCount: 12
      shards: 1
      message: ""
      executedActions: []
      ledgerRevision: sha256:9f3c
  lastAppliedSnapshot:
    - stage: infrastructure
      url: http://source-controller.../infra.tar.gz
      digest: sha256:9f3c
  pendingUpdate:               # set only when a window holds a rollout
    revisions: { payments/payments-app: sha256:cafe }
    nextWindowOpens: "2026-06-16T08:00:00Z"
  lastHandledUpdateOverride: "2026-06-15T09:30:00Z"
```

The `Ready` condition's reason is one of the wire-stable values documented in the
[runbooks](/runbooks/). The happy-state reason is `Succeeded`, matching the
apply-style success reason kustomize-controller writes, so existing Flux tooling
and alert routing recognize it unchanged.
