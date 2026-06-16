---
title: stagesetctl build
description: Render a StageSet's manifests to stdout, exactly as the controller would.
tags: [cli, stages, sources]
---

Runs the same resolve → fetch → build pipeline the controller uses and writes the
result — a multi-document YAML stream — to stdout. This is what would be applied,
before it is applied. To preview the change against live cluster state instead, use
[`diff`](/cli/diff/).

```text
stagesetctl build NAME [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--stage` | _(all)_ | Render only the named stage(s); repeatable. |
| `--source-dir` | _(none)_ | Use a local artifact tree as `[STAGE=]PATH` instead of fetching from the cluster; repeatable. |
| `--show-secrets` | `false` | Reveal Secret values instead of masking them. |
| `--as-tenant` | `false` | Render impersonating the StageSet's `spec.serviceAccountName` (see [multi-cluster and tenancy](/usage/multi-cluster/)). |

Secret values are masked by default, so the output is safe to paste into a review.
`build` writes YAML unconditionally — there is no output-format flag.

## Example

```shell
stagesetctl build payments --stage application
```

```yaml
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: payments
spec:
  replicas: 6
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
        - name: web
          image: registry.internal/web:2.1.0
---
apiVersion: v1
kind: Secret
metadata:
  name: web-config
  namespace: payments
type: Opaque
data:
  token: '***'          # masked; pass --show-secrets to reveal
```

`--source-dir` makes `build` work offline — point it at the directory an artifact
would have unpacked to and it skips the cluster fetch, for authoring and CI. The
value is `[STAGE=]PATH`: prefix a stage name to target one stage, or give a bare
path to feed every stage that has no entry of its own. Repeat the flag to map
each stage to its own tree:

```shell
# one stage from a local tree
stagesetctl build payments --stage application --source-dir application=./out

# every stage from one tree (bare path), overriding just infrastructure
stagesetctl build payments \
  --source-dir ./checkout \
  --source-dir infrastructure=./infra-checkout
```
