---
name: stageset
description: >-
  Author and operate StageSet custom resources (apiVersion stages.metio.wtf/v1)
  for the stageset-controller — a Flux controller for ordered, gated, multi-stage
  Kubernetes delivery. Use this when writing or editing a StageSet YAML, wiring a
  Flux source (GitRepository / OCIRepository / Bucket / ExternalArtifact, or a
  producer like a JaaS JsonnetSnippet) into staged rollouts,
  adding typed actions / ready checks / update windows / versioned migrations /
  conflict policies, configuring per-tenant impersonation, or driving a StageSet
  with the stagesetctl CLI (diff, build, get, reconcile). Applies whenever a repo
  has StageSet manifests or the stageset-controller is in play.
allowed-tools: Bash(stagesetctl *), Bash(kubectl *)
---

# Using StageSet

`StageSet` (`stages.metio.wtf/v1`) is a Flux-compatible controller for **ordered,
gated, multi-stage delivery**. A StageSet rolls out a sequence of **stages**, each
built from a Flux source (`GitRepository`, `OCIRepository`, `Bucket`, or an
`ExternalArtifact`), waiting for each stage to become healthy
before the next — with typed actions between stages, update windows, versioned
migrations, conflict policies, and per-stage pruning. It is continuously
reconciled and applies under per-tenant ServiceAccount impersonation.

Reach for a StageSet (over a plain Flux `Kustomization`) when a release must happen
**in order**: CRDs before the operator that needs them, a migration before the app,
a gate before a production rollout.

## The docs are the source of truth

The full, current documentation lives at <https://stageset.projects.metio.wtf/>,
with a machine-readable index at `/llms.txt` and the whole site concatenated at
`/llms-full.txt`. When you need an exact field, default, or example, prefer those
over memory. Key pages: the [API reference](https://stageset.projects.metio.wtf/api/stageset/),
[usage](https://stageset.projects.metio.wtf/usage/) (one feature each), and
[CLI](https://stageset.projects.metio.wtf/cli/).

`references/reference.md` in this skill is a compact cheat-sheet of the same.

## Authoring a StageSet

Start minimal — only `spec.stages` is required (`spec.interval` is optional and
defaults to the controller's `--default-interval`):

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
        name: my-app          # an ExternalArtifact; kind defaults to ExternalArtifact
```

Then layer options on, in roughly this order of need:

- **More stages** — they run top-to-bottom; each waits for the previous to be Ready.
- **`serviceAccountName`** — impersonated for every apply/prune/action. Set it in
  multi-tenant/production clusters; the StageSet can only do what that SA allows.
- **`decryption`** — `{provider: sops, secretRef: {name}}` decrypts SOPS-encrypted
  files in stage sources before they apply. Keys in the Secret: age under `*.agekey`,
  PGP under `*.asc` (both tenant-scoped, read under `serviceAccountName`); cloud KMS
  uses the controller's ambient creds and needs no `secretRef`.
- **Per-stage build surface** — `path`, `prune` (default true), `patches`
  (Kustomize), `postBuild.substitute` / `substituteFrom`.
- **`actions`** (`pre` / `post` / `onFailure`) — each Action has **exactly one** of
  `patch` / `http` / `wait` / `job` / `delete` / `apply`.
- **`readyChecks`** — `checks` (kstatus) and/or `exprs` (CEL) to define "healthy".
- **`conflictPolicy`** — `default` + per-resource `rules` (`Fail`/`Recreate`/
  `KeepExisting`); `allowDataLoss: true` is required to `Recreate` a PVC/PV.
- **`updateWindows`** + `windowScope` — gate *when* new revisions roll out.
- **`version`** + `migrations` — run a migration once when crossing a version boundary.
- **`rollbackOnFailure`** — restore the last good revision on failure (needs a
  rollback store configured on the controller).

### Gotchas to honor

- `sourceRef.kind` defaults to `ExternalArtifact`. A stage can also point **directly**
  at a classic Flux source — `GitRepository`, `OCIRepository`, `Bucket` — so plain
  manifests in Git/OCI/Bucket need no producer. Use a producer (e.g. a JaaS
  `JsonnetSnippet`, `apiVersion: jaas.metio.wtf/v1`) only when the manifests must be
  *rendered* first; the controller resolves the producer's published ExternalArtifact.
- An `Action` with two operation blocks is rejected — exactly one.
- No labels/annotations are needed for the controller to watch sources; the
  `sourceRef` is the link.
- The chart is the only supported install; configure the controller via Helm values.

## Driving with stagesetctl

`stagesetctl` previews and drives StageSets with your kubeconfig (also works as
`kubectl stageset`). Always **preview before applying logic changes**:

```bash
stagesetctl diff my-app -n apps        # what would change (exit 1 = changes; CI gate)
stagesetctl build my-app --stage app   # render the manifests to stdout
stagesetctl get my-app -n apps         # human-readable status (stages, phases, revisions)
stagesetctl reconcile my-app --wait    # force an out-of-band reconcile
```

`diff` follows the diff(1) convention (exit 1 on changes), so it gates CI. Use
`stagesetctl reconcile --update-now` to push a window-held rollout through.

## Debugging a failed StageSet

`status.conditions[Ready].reason` names the failure; each reason has a runbook at
`https://stageset.projects.metio.wtf/runbooks/<reason>/` (lower-cased). `kubectl
describe stageset <name>` shows the per-stage phase and message; `status.stages[]`
carries each stage's phase, applied revision, and executed actions.
