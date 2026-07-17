---
title: Actions
description: Typed pre, post, and on-failure steps — jobs, HTTP gates, waits, patches, deletes, transient applies.
tags: [actions, stages, ready-checks]
---

Actions are typed steps the controller runs around a stage's apply. They turn an
ordered apply into an orchestrated rollout — run a migration before the app, gate
the stage on an external check, clean up on failure.

A stage has three action hooks:

- **`pre`** — run before the manifests are built and applied. A failure aborts the
  stage with nothing applied.
- **`post`** — run after the apply is verified. The stage is `Ready` only if these
  all succeed.
- **`onFailure`** — best-effort steps run on any failure from the apply onward.

A stage's `onFailure` fires at the *moment* of failure, before any rollback. For
cleanup that must run *after* the previous manifests are restored, use the
StageSet-level [`onRollback`](#post-rollback-cleanup-onrollback) hook instead.

Each action has a `name`, optional `timeout` and `retries`, and **exactly one**
operation type (`patch`, `http`, `wait`, `job`, `delete`, or `apply`) — enforced
by the validating admission webhook. Actions within a hook run in list order.

```yaml
spec:
  stages:
    - name: application
      sourceRef:
        name: my-app
      actions:
        pre:
          - name: db-migrate
            timeout: 10m
            job:
              sourceRef:
                name: my-app-migrations    # render & await Jobs from this artifact
        post:
          - name: smoke-test
            retries: 3
            http:
              url: https://my-app.internal/healthz
              expectedStatus: [200]
        onFailure:
          - name: page-oncall
            http:
              url: https://alerts.internal/stageset-failed
              method: POST
```

## The six action types

### `job`

Render and await Kubernetes Jobs from an artifact. The classic use is a database
migration that must complete before the app is applied.

```yaml
- name: db-migrate
  job:
    sourceRef:
      name: my-app-migrations
    path: ./jobs
```

### `http`

Call an HTTP endpoint and gate on the response — an approval webhook, a smoke
test, an external readiness probe. Hosts must be permitted by the controller's
`--allowed-action-hosts`; loopback and link-local are always denied. `method`
defaults to `POST`; `expectedStatus` defaults to any `2xx`. The body and headers
can be read from a `Secret` so tokens never sit in the spec:

```yaml
- name: smoke-test
  http:
    url: https://my-app.internal/healthz
    method: GET
    expectedStatus: [200]
    headersFrom:
      - name: gate-token       # Secret name
        key: authorization     # the key names the header; its value is the value
    bodyFrom:
      name: gate-payload       # Secret name
      key: body                # this key's value becomes the request body
```

### `wait`

Block for a fixed duration, or until a [CEL](https://github.com/google/cel-spec)
expression holds against a target object.

```yaml
- name: settle
  wait:
    duration: 30s
- name: until-available
  wait:
    target:
      apiVersion: apps/v1
      kind: Deployment
      name: web
    expr: "status.availableReplicas >= 3"
    timeout: 5m
```

### `patch`

Patch an existing object — flip a feature flag, scale something, annotate. `type`
is `merge` (default) for a strategic-merge patch, or `json6902` for a JSON Patch:

```yaml
- name: enable-traffic
  patch:
    target:
      apiVersion: v1
      kind: Service
      name: web
    type: merge               # default; or json6902
    patch: |
      { "spec": { "selector": { "release": "green" } } }
```

Instead of a single `name`, the target can carry a `selector` to patch **every**
object of that kind whose labels match — applied to each in the namespace. Exactly
one of `name` or `selector` is set.

```yaml
- name: grow-data-volumes
  patch:
    target:
      apiVersion: v1
      kind: PersistentVolumeClaim
      selector:
        matchLabels:
          app: web            # label your StatefulSet's volumeClaimTemplates so its PVCs carry this
    patch: |
      { "spec": { "resources": { "requests": { "storage": "100Gi" } } } }
```

This is the missing half of resizing a StatefulSet's storage: the
`volumeClaimTemplates` are immutable and its existing per-ordinal PVCs aren't
touched by the StatefulSet, so a selector patch grows the existing volumes (the
StorageClass needs `allowVolumeExpansion: true`; storage can only grow, never
shrink), while a [`delete`](#delete) with `cascade: Orphan` plus a re-apply
recreates the StatefulSet so new ordinals pick up the larger template. Some CSI
drivers expand the filesystem only on pod restart, so this isn't always
zero-downtime.

### `delete`

Remove an existing object; a missing object counts as success.

```yaml
- name: drop-old-job
  delete:
    target:
      apiVersion: batch/v1
      kind: Job
      name: legacy-migration
```

`cascade` controls the target's dependents, mirroring `kubectl delete --cascade`:

- `Background` (default) — delete the object; garbage-collect dependents asynchronously.
- `Foreground` — delete dependents first, then the object.
- `Orphan` — delete the object but leave its dependents running (their controller `ownerReferences` are removed).

**Recreating an object whose immutable fields changed, without downtime.** Some
fields can't be `apply`d in place — a StatefulSet's `serviceName` or
`podManagementPolicy`, a Service's `clusterIP`, a Job's `template`. `cascade: Orphan`
is the building block: orphan-delete the object so its pods keep running, then let
the stage re-apply the new spec, which **adopts** the still-running pods by selector.

```yaml
stages:
  - name: app
    sourceRef:
      name: my-app          # carries the new StatefulSet spec
    actions:
      pre:
        - name: orphan-old-sts
          delete:
            target:
              apiVersion: apps/v1
              kind: StatefulSet
              name: web
            cascade: Orphan   # keep the pods; the apply below re-adopts them
```

Doing it in `pre` of the same stage that re-applies keeps the orphan window
tight — the pods are ownerless only between the delete and the stage's apply in the
same reconcile. (A delete in one stage and the re-apply in a later stage works too,
but the pods stay ownerless across the gate between them.)

Caveats: adoption needs the new object's selector to still match the running pods,
so this can't help when the **selector itself** is the immutable change. And
`Orphan` keeps existing PVCs — changing a StatefulSet's `volumeClaimTemplates`
this way leaves the old volumes in place for existing ordinals (it doesn't resize
storage).

### `apply`

Apply transient, rollout-scoped manifests that are **not** inventory-tracked and
are never pruned — a maintenance page, a one-shot canary, a temporary config. With
`wait: true` the action blocks until the applied objects report Ready (kstatus),
bounded by the action `timeout`, so a following `patch` can repoint traffic only
once the resource is serving.

Because the applied objects are never pruned by the inventory diff, stand a
resource up only for the duration of a rollout by pairing an `apply` in `pre` with
a matching `delete` in `post`, and guard a mid-run crash with an `onFailure`
delete:

```yaml
actions:
  pre:
    - name: stand-up-maintenance-page
      apply:
        sourceRef:
          name: maintenance-page    # an ExternalArtifact holding a Pod + Service
        wait: true                  # block until it is serving
  post:
    - name: tear-down-maintenance-page
      delete:
        target:
          apiVersion: v1
          kind: Pod
          name: maintenance-page
  onFailure:
    - name: tear-down-maintenance-page-on-failure
      delete:
        target:
          apiVersion: v1
          kind: Pod
          name: maintenance-page
```

The action ledger gates each step per pinned revision, so a retry or controller
restart never re-applies or re-deletes the resource for the same snapshot.

## Scope: run per revision or per version

By default a pre/post action runs once per pinned **revision** — so any change
that produces a new artifact revision, including a ConfigMap-only edit, re-runs
it. For upgrade choreography (`enable-maintenance-mode` → `db-upgrade` →
`purge-caches`), that over-fires: the ladder is only meaningful when the deployed
*version* changes, yet a config push re-runs the whole thing.

Set `scope: Version` on the action to key it to the resolved `spec.version`
instead of the revision:

```yaml
stages:
  - name: app
    sourceRef: { name: moodle-app }
    actions:
      pre:
        - name: render-config-check
          scope: Revision          # default; re-runs on any revision change
          job: { sourceRef: { name: moodle-job-check } }
        - name: db-upgrade
          scope: Version           # runs once per version, not per revision
          job: { sourceRef: { name: moodle-job-upgrade } }
```

- `scope: Revision` (the default) — once per pinned revision, as above.
- `scope: Version` — once per **version episode**: a run of reconciles over which
  the resolved `spec.version` is unchanged. A config-only revision bump at a
  fixed version does not re-run it; crossing a version boundary does. It requires
  `spec.version` and is rejected without one.

`scope` is valid only on `pre` and `post` actions. `onFailure` and `onRollback`
are ledger-exempt (they run on every failure or rollback), and migration actions
are already keyed to their version transition — a `scope` on any of these is
rejected at admission.

Adoption is handled the same way versioning handles it: the first reconcile of a
versioned StageSet records the version and treats every `scope: Version` action
as already done, so migrating a running fleet in does not fire its maintenance
choreography. A version-scoped action first runs when the version next changes.

To run a `job` action only when the deployed version crosses a release boundary
(with dependency ordering and once-per-transition semantics), see
[versioned migrations](/gating/versioned-migrations/); `scope: Version` is the
lighter tool for recurring per-release choreography that has no boundary to name.

## Post-rollback cleanup (`onRollback`)

A stage's `onFailure` actions run at the moment the stage fails — *before* the
controller rolls anything back. That is the wrong moment for cleanup that only
makes sense once the previous manifests are back: a failed upgrade that turned on
an application maintenance mode still needs that mode lifted *after* the old
version is restored, not while the broken one is still live.

The StageSet-level `spec.onRollback` list runs best-effort **after** a rollback
has restored the previous revision — both the whole-run
[`rollbackOnFailure`](/gating/rollback/) rollback and a promotion gate's
single-stage `onFailure: Rollback` revert. The actions run against the restored
state, under `spec.serviceAccountName`, and use the same six operation types as
stage actions.

Unlike stage actions, `onRollback` is **not** gated by the per-revision action
ledger — it fires on *every* rollback, so a cleanup step like lifting a
maintenance mode always runs even if the same revision is rolled back repeatedly.
A failure only emits a Warning event; it never blocks the rollback report.

```yaml
spec:
  rollbackOnFailure: true
  serviceAccountName: deployer
  onRollback:
    - name: disable-maintenance-mode
      timeout: 5m
      job:
        sourceRef:
          name: moodle-maintenance-off   # a Job that runs `php admin/cli/maintenance.php --disable`
  stages:
    - name: moodle
      sourceRef:
        name: moodle
      actions:
        pre:
          - name: enable-maintenance-mode
            job:
              sourceRef:
                name: moodle-maintenance-on
```

If the upgrade of the `moodle` stage fails after maintenance mode was switched on,
`rollbackOnFailure` restores the previous manifests and then `onRollback` runs the
`disable-maintenance-mode` job — so the site comes back on the old version instead
of being stranded in maintenance mode. Action `name`s must be unique within the
`onRollback` list.
