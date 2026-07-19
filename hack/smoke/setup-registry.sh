#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Deploy a plain-HTTP OCI registry in-cluster for the image-verification scenario.
# The registry is reachable by the controller pod at
# registry.<ns>.svc.cluster.local:5000 (a ClusterIP Service) and, once the scenario
# port-forwards to it, by the host at localhost:5000 for pushing + signing. It has no
# TLS, so the controller must be told to treat it as insecure
# (--image-verification-insecure-registry); the kubelet never pulls from it because
# the scenario's Deployments run zero replicas.
set -euo pipefail

NS="${1:-image-verify}"
# Pinned, long-form per repo convention. distribution 2.8.3 serves the OCI referrers
# API; go-containerregistry and cosign also agree via the tag fallback, so bundle
# discovery does not hinge on the exact registry version.
REGISTRY_IMAGE="docker.io/library/registry:2.8.3"

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: registry
  namespace: ${NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: registry
  template:
    metadata:
      labels:
        app: registry
    spec:
      containers:
        - name: registry
          image: ${REGISTRY_IMAGE}
          ports:
            - containerPort: 5000
          env:
            - name: REGISTRY_STORAGE_DELETE_ENABLED
              value: "true"
---
apiVersion: v1
kind: Service
metadata:
  name: registry
  namespace: ${NS}
spec:
  selector:
    app: registry
  ports:
    - port: 5000
      targetPort: 5000
EOF

kubectl -n "$NS" rollout status deploy/registry --timeout=120s
