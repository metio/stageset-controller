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

To run a `job` action only when the deployed version crosses a release boundary,
see [versioned migrations](/usage/versioned-migrations/).
