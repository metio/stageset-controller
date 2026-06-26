---
title: Helm chart values
description: Complete reference for every value the stageset-controller Helm chart exposes, generated from the chart's values.schema.json.
tags: [installation, helm, chart, values, reference]
---

The stageset-controller Helm chart lives in the
[metio/helm-charts](https://github.com/metio/helm-charts/tree/main/charts/stageset-controller)
monorepo and is published at `oci://ghcr.io/metio/helm-charts/stageset-controller`.
The table below is generated from the chart's `values.schema.json`, so it tracks
the chart's current schema rather than a hand-maintained copy.

For how the values map onto the binary's runtime behaviour, see the
[Configuration reference](/reference/configuration/) — every `controller.*`
value drives the corresponding `--flag`.

{{< helm-values data="helm-values" >}}
