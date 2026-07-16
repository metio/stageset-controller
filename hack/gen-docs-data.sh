#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Generates every docs/data/* file the website's data-driven pages render:
#   - docs/data/flags.json       — the binary's CLI flags (via hack/flaggen)
#   - docs/data/helm-values.json — the stageset-controller chart's flattened
#                                  values schema
#
# Run this through the flake's development shell before building the site:
#   nix develop --command hack/gen-docs-data.sh
#   nix develop --command serve
#
# The schema is generated on-the-fly from the chart's Chart.yaml + values.yaml
# fetched from helm-charts' main branch (helm-schema, the same tool the chart
# repo runs at package time — the schema is never committed there). The docs
# workflow's daily schedule re-runs this, so a chart change reaches the site with
# no cross-repo trigger.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

data_dir="docs/data"
mkdir -p "${data_dir}"

charts_base="https://raw.githubusercontent.com/metio/helm-charts/main/charts"

echo "==> flaggen → ${data_dir}/flags.json"
go run ./hack/flaggen -o "${data_dir}/flags.json"

# Fetch a chart's Chart.yaml + values.yaml into a tempdir, generate its
# values.schema.json with helm-schema, then flatten it for the docs data file.
flatten_schema() {
  local chart="$1" out="$2"
  echo "==> ${chart} values.schema.json → ${out}"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' RETURN
  for f in Chart.yaml values.yaml; do
    curl --fail --silent --show-error --location \
      "${charts_base}/${chart}/${f}" -o "${tmp}/${f}"
  done
  helm-schema -c "${tmp}" -k additionalProperties
  jq -f hack/flatten-schema.jq "${tmp}/values.schema.json" > "${out}"
}

flatten_schema stageset-controller "${data_dir}/helm-values.json"

echo "==> docs data generated"
