---
title: StageSet Controller
---

# StageSet Controller

`stageset-controller` is a [Flux](https://fluxcd.io/) controller for ordered, gated, multi-stage delivery.

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
gate rollouts behind [update windows](/usage/update-windows/), and run
version-aware [migrations](/usage/versioned-migrations/) when you cross a release boundary.
Everything is reconciled continuously, drift-corrected, and pruned with ApplySet
semantics.

## What a StageSet looks like

The smallest useful StageSet is one stage pointing at one artifact:

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
        name: my-app          # an ExternalArtifact in this namespace
```

The same shape scales up to a gated rollout:

```yaml
apiVersion: stages.metio.wtf/v1
kind: StageSet
metadata:
  name: payments
  namespace: payments
spec:
  serviceAccountName: payments-deployer     # every apply is impersonated as this SA

  stages:
    # 1 ── shared infrastructure: CRDs, namespaces, RBAC
    - name: infrastructure
      sourceRef:
        name: payments-infra                # an ExternalArtifact
      readyChecks:
        checks:
          - apiVersion: apiextensions.k8s.io/v1
            kind: CustomResourceDefinition
            name: ledgers.payments.example

    # 2 ── the application, started only once infrastructure is Ready
    - name: application
      sourceRef:
        name: payments-app
      actions:
        pre:
          - name: db-migrate                # runs before the manifests are applied
            job:
              sourceRef:
                name: payments-migrations
        post:
          - name: smoke-test                # stage is Ready only if this passes
            http:
              url: https://payments.internal/healthz
              expectedStatus: [200]

  # new revisions roll out only outside the Friday-evening change freeze
  updateWindows:
    - type: Deny
      schedule: "0 17 * * FRI"
      duration: 60h
      timeZone: Europe/Berlin
```

Stages run top to bottom. `infrastructure` must report Ready (its CRD established)
before `application` is touched; the migration Job runs before the app is applied;
the rollout is held when the change-freeze window is open. Everything is
continuously reconciled — drift is corrected, removed objects are pruned.

## Where to go next

- **[Installation](/installation/)** — install on Kubernetes, then harden for
  production and wire up observability.
- **[Usage](/usage/)** — worked examples for every feature, from a single stage
  to versioned migrations.
- **[CLI](/cli/)** — `stagesetctl` for previewing (`diff`), rendering (`build`),
  and driving (`reconcile`) StageSets.
- **[API reference](/api/)** — every field of every custom resource, explained.
- **[Comparisons](/comparisons/)** — how StageSet relates to Helm, Kustomize,
  Tanka, kubecfg, and plain Flux.
- **[Runbooks](/runbooks/)** — symptom → cause → remediation for every status
  reason.

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
