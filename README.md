<!--
SPDX-FileCopyrightText: The stageset-controller Authors
SPDX-License-Identifier: 0BSD
-->

# stageset-controller

A [Flux](https://fluxcd.io)-compatible Kubernetes controller for **ordered, gated,
multi-stage delivery**. A `StageSet` rolls out a sequence of stages — each built
from a Flux source (a `GitRepository`, `OCIRepository`, `Bucket`, or an
[`ExternalArtifact`](https://fluxcd.io/flux/components/source/externalartifacts/)) —
waiting for each stage to become healthy before the next begins, with typed
pre/post actions, update windows, versioned migrations, and per-stage pruning.
It is continuously reconciled, drift-corrected, and applied under per-tenant
impersonation.

**📖 Documentation — installation, usage, API reference, tutorials, and
contributing — lives at <https://stageset.projects.metio.wtf/>.**

Licensed under [0BSD](LICENSE); the repository is [REUSE](https://reuse.software/)
compliant.
