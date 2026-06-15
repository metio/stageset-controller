#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Installs a minimal stub of Flux's ExternalArtifact CRD so the smoke runs
# without a live source-controller. The controller only reads
# status.artifact.{url,revision,digest} and status.conditions, so an
# unstructured (x-kubernetes-preserve-unknown-fields) schema is enough — the
# scenarios stamp those fields directly. A real cluster gets the typed CRD from
# Flux v2.7.0+; the contract under test (resolve -> fetch -> verify) is identical.
set -euo pipefail

cat <<'EOF' | kubectl apply -f -
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: externalartifacts.source.toolkit.fluxcd.io
spec:
  group: source.toolkit.fluxcd.io
  scope: Namespaced
  names:
    kind: ExternalArtifact
    listKind: ExternalArtifactList
    plural: externalartifacts
    singular: externalartifact
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
  crd/externalartifacts.source.toolkit.fluxcd.io --timeout=60s
