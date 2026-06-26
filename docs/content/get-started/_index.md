---
title: Get started
description: Install the controller and roll out your first StageSet, then go from plain manifests to a Jsonnet-rendered, gated rollout.
tags: [getting-started, installation, quickstart]
---

Stand up the controller and ship your first release. Start with the Quickstart
and work down — each page builds on the previous one.

- **[Quickstart](/get-started/quickstart/)** — from an empty cluster to one
  running StageSet, the shortest path.
- **[Install on Kubernetes](/get-started/kubernetes/)** — prerequisites and the
  Helm install in full.
- **[From Jsonnet to a gated rollout](/get-started/jsonnet-to-rollout/)** — the
  flagship flow: render Jsonnet with [JaaS](https://jaas.projects.metio.wtf/),
  gate with readiness checks, add versioned migrations.
- **[Stage sources](/get-started/flux-sources/)** — point a stage at a
  `GitRepository`, `OCIRepository`, `Bucket`, or a rendered `ExternalArtifact`.
</content>
