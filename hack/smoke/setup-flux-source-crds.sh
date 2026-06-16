#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Installs minimal stubs of Flux's classic source CRDs (GitRepository,
# OCIRepository, Bucket) so the direct-source smoke runs without a live
# source-controller. The controller reads the same status.artifact +
# status.conditions contract from these as from an ExternalArtifact, so an
# unstructured schema is enough — the scenario stamps those fields directly. A
# real cluster gets the typed CRDs from Flux; the contract under test is identical.
set -euo pipefail

for entry in "GitRepository:gitrepositories" "OCIRepository:ocirepositories" "Bucket:buckets"; do
  kind="${entry%%:*}"
  plural="${entry##*:}"
  cat <<EOF | kubectl apply -f -
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ${plural}.source.toolkit.fluxcd.io
spec:
  group: source.toolkit.fluxcd.io
  scope: Namespaced
  names:
    kind: ${kind}
    listKind: ${kind}List
    plural: ${plural}
    singular: $(printf '%s' "$kind" | tr '[:upper:]' '[:lower:]')
  versions:
    - name: v1
      served: true
      storage: true
      subresources:
        status: {}
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
EOF
  kubectl wait --for=condition=Established \
    "crd/${plural}.source.toolkit.fluxcd.io" --timeout=60s
done
