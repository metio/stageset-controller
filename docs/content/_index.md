---
title: StageSet Controller
---

# StageSet Controller

`stageset-controller` is a [Flux](https://fluxcd.io/) controller for **ordered,
gated, multi-stage delivery** — roll a release out one stage at a time, prove
each stage healthy before the next begins, and gate the whole thing on
schedules, approvals, and error budgets. It is GitOps-native: every stage reads
a Flux source, and the whole release is reconciled continuously, drift-corrected,
and pruned.

Flux's `kustomize-controller` and `helm-controller` apply an artifact in one
shot. That fits most releases, but not one that has to happen in sequence:
install the CRDs before the operator that needs them, run a database migration
before the app that reads the new schema, hold a production rollout until the
canary is healthy, freeze changes during business hours.

A `StageSet` describes a release as an ordered list of stages. Each stage applies
a Flux source — a `GitRepository`, `OCIRepository`, `Bucket`, or an
[`ExternalArtifact`](https://fluxcd.io/flux/components/source/externalartifacts/)
(including one rendered on the fly by a producer like [JaaS](https://jaas.projects.metio.wtf/)) —
waits for it to become healthy, and only then lets the next stage begin. Between
stages, run typed actions (a migration `Job`, an HTTP gate, a wait-for-condition),
gate rollouts behind [update windows](/gating/update-windows/), and run
version-aware [migrations](/gating/versioned-migrations/) when you cross a release boundary.
Everything is reconciled continuously, drift-corrected, and pruned from a precise
per-stage inventory.

## What you can do with it

### Sequence and gate every step

- **[Ordered stages](/defining-a-release/stages-and-sources/)** — each stage applies a source
  and must report healthy before the next one starts.
- **[Ready checks](/defining-a-release/ready-checks/)** — kstatus and CEL gates, so a stage
  counts as done only when its objects are genuinely ready, not merely applied.
- **[Typed actions](/defining-a-release/actions/)** — run a migration `Job`, an HTTP gate, a
  wait-for-condition, or a patch/delete before, after, or on failure of a stage.
- **[Action scopes](/defining-a-release/actions/#scope-revision-version-or-lifetime)** —
  run an action once per revision, once per application *version* (so config churn
  stops re-running upgrade choreography), or once *ever* — a durable bootstrap,
  recorded in a ledger that survives a delete-and-recreate.
- **[Promotion gates](/gating/stage-promotion/)** — hold a stage behind a soak
  window, a manual approval, or a metric analysis before it advances.

### Roll out safely

- **[Update windows](/gating/update-windows/)** — allow or deny new revisions on a
  schedule, so a change freeze holds without pausing drift correction.
- **[Error-budget freeze](/gating/error-budget/)** — pause rollouts automatically
  while a service is out of its SLO budget, and resume when it recovers.
- **[Versioned migrations](/gating/versioned-migrations/)** — run version-aware
  migrations when a release crosses a boundary, with approval gates and coverage
  checks.
- **[Automatic rollback](/gating/rollback/)** — restore the last-good revision
  when a rollout fails.

### Source-agnostic and Flux-native

- **[Any Flux source](/defining-a-release/stages-and-sources/)** — `GitRepository`,
  `OCIRepository`, `Bucket`, or `ExternalArtifact`, including one rendered on the
  fly by a producer like [JaaS](https://jaas.projects.metio.wtf/).
- **[Producer-aware](/integrations/producer-aware-sources/)** — re-render a stage
  automatically when its upstream producer republishes.
- **[Secrets encryption](/security/encryption/)** — decrypt SOPS-encrypted manifests
  in memory during apply.

### Built for platform teams

- **[Multi-tenancy](/security/multi-cluster/)** — every apply runs as a tenant
  `ServiceAccount`, so the controller needs no broad write access of its own.
- **[Observability](/observability/)** — Prometheus metrics, SLOs, OTLP tracing,
  a starter alert set, and a Kubernetes Event on every transition.
- **[Tooling](/cli/)** — a `stagesetctl` CLI to preview (`diff`), render
  (`build`), and drive (`reconcile`) StageSets, plus an
  [MCP server](/integrations/mcp-server/) for agent-driven operation.

## What a StageSet looks like

A single StageSet can express an entire gated release — ordered stages, a
producer-rendered source, typed actions, promotion gates, versioned migrations,
an error-budget freeze, and rollback — all declaratively:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: payments
  namespace: payments
spec:
  serviceAccountName: payments-deployer    # every apply runs as this tenant SA

  decryption:                              # decrypt SOPS-encrypted Secrets in-line
    provider: sops
    secretRef:
      name: sops-age

  rollbackOnFailure: true                  # restore the last-good revision on failure

  # track the deployed version from the app's own label, and run a one-off
  # migration when a rollout crosses the 2.0.0 boundary
  version:
    fromObject:
      stage: application
      kind: Deployment
      name: payments-api
  migrations:
    - name: ledger-schema-2-0
      from: ">=1.0.0, <2.0.0"              # current version this applies to
      to: "2.0.0"                          # the exact boundary it crosses
      stage: application
      actions:
        - name: migrate-ledger
          job:
            sourceRef:
              name: payments-migrations

  # new revisions roll out only outside the Friday-evening freeze ...
  updateWindows:
    - type: Deny
      schedule: "0 17 * * FRI"
      duration: 60h
      timeZone: Europe/Berlin

  # ... and only while the service is within its SLO error budget
  errorBudget:
    source:
      prometheus:
        address: http://prometheus.monitoring:9090
        query: slo:period_error_budget_remaining:ratio{sloth_service="payments"}
    freezeThreshold: "0"                    # freeze once the budget is overspent
    resumeThreshold: "0.05"                 # resume only after it recovers

  stages:
    # 1 ── shared infrastructure: CRDs, namespaces, RBAC
    - name: infrastructure
      sourceRef:
        name: payments-infra               # an ExternalArtifact (the default kind)
      readyChecks:
        checks:                            # gate on the CRD being Established
          - apiVersion: apiextensions.k8s.io/v1
            kind: CustomResourceDefinition
            name: ledgers.payments.example

    # 2 ── the application, rendered on the fly from Jsonnet by JaaS
    - name: application
      sourceRef:
        kind: JsonnetSnippet               # a producer, resolved to its ExternalArtifact
        apiVersion: jaas.metio.wtf/v1
        name: payments-api
      actions:
        pre:
          - name: db-migrate               # runs before the manifests are applied
            job:
              sourceRef:
                name: payments-migrations
        post:
          - name: smoke-test               # stage is Ready only if this passes
            http:
              url: https://payments.internal/healthz
              expectedStatus: [200]
      promotion:
        soak: 15m                          # advance only if it stays healthy ...
        analysis:                          # ... and the 5xx rate stays low
          checks:
            - name: error-rate
              source:
                prometheus:
                  address: http://prometheus.monitoring:9090
                  query: sum(rate(http_requests_total{app="payments",code=~"5.."}[1m]))/sum(rate(http_requests_total{app="payments"}[1m]))
              threshold:
                max: "0.01"                # ≤ 1% 5xx

    # 3 ── production, promoted only after a human confirms
    - name: prod
      sourceRef:
        name: payments-prod
      promotion:
        requireManualPromotion: true
```

Stages run top to bottom: `infrastructure` must be Ready before `application` is
touched, the migration runs as the version crosses `2.0.0`, `application` soaks
and is checked against its error rate before `prod` begins, and `prod` waits for a
manual promotion. New revisions roll out only outside the Friday freeze and while
the SLO is in budget, and a failed run is rolled back — all reconciled
continuously, with drift corrected and removed objects pruned. Only `spec.stages`
is required; everything else is optional, so the
[Quickstart](/get-started/quickstart/) starts from a single stage.

## Where to go next

- **[Get started](/get-started/quickstart/)** — go from an empty cluster to one
  running StageSet in a few steps.
- **[Guides](/guides/)** — end-to-end walkthroughs: parameterizing a rollout,
  progressive delivery, generating migrations with Jsonnet.
- **[Defining a release](/defining-a-release/)** and
  **[Gating & rollout safety](/gating/)** — a page per feature, from a single
  stage to error-budget freezes, with copy-pasteable examples.
- **[CLI](/cli/)** and **[API reference](/api/)** — `stagesetctl` commands and
  every field of every custom resource.
- **[Comparisons](/comparisons/)** — how StageSet relates to Helm, Kustomize,
  Tanka, kubecfg, Flux, Argo Rollouts, and more.
- **[Runbooks](/runbooks/)** — symptom → cause → remediation for every status
  reason and operational alert.

## Related projects

`stageset-controller` handles the delivery end and composes with two adjacent
projects, each useful on its own:

- **[JOI](https://github.com/metio/jsonnet-oci-images)** publishes Jsonnet
  libraries as single-layer OCI images.
- **[JaaS](https://jaas.projects.metio.wtf/)** evaluates Jsonnet on demand and
  publishes the result as a Flux `ExternalArtifact`.
- `stageset-controller` takes those artifacts and rolls them out, in order, with
  gates.

JOI and JaaS are not required — a stage reads straight from a `GitRepository`,
`OCIRepository`, or `Bucket`, or from any `ExternalArtifact`, whatever produced
it.
