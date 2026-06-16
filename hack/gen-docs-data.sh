#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Generates every docs/data/* file the website's data-driven pages render:
#   - docs/data/flags.json       — the binary's CLI flags (via hack/flaggen)
#   - docs/data/helm-values.json — the stageset-controller chart's flattened
#                                  values schema
#
# Run this in the Go/ilo shell before building the site, e.g.:
#   ilo bash -c 'hack/gen-docs-data.sh'
#   ilo --no-rc @dev/serve
#
# The schema is fetched from helm-charts' main branch so the published reference
# always tracks the latest chart. The docs workflow's daily schedule re-runs
# this, so a chart change reaches the site with no cross-repo trigger.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

data_dir="docs/data"
mkdir -p "${data_dir}"

charts_base="https://raw.githubusercontent.com/metio/helm-charts/main/charts"

echo "==> flaggen → ${data_dir}/flags.json"
go run ./hack/flaggen -o "${data_dir}/flags.json"

echo "==> stageset-controller values.schema.json → ${data_dir}/helm-values.json"
curl --fail --silent --show-error --location \
  "${charts_base}/stageset-controller/values.schema.json" \
  | jq -f hack/flatten-schema.jq > "${data_dir}/helm-values.json"

echo "==> docs data generated"
