#!/usr/bin/env bash
# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD
#
# Deploys Adobe S3Mock into the `s3mock` namespace. S3Mock is an in-memory,
# zero-credentials S3 server: it accepts any (or no) signature, so it backs the
# controller's S3 rollback store in anonymous mode (rollbackStore.s3.anonymous).
# `initialBuckets` is created at startup, so the `stageset-rollback` bucket the
# rollback store writes into exists before the controller is installed. S3Mock
# only FAKES server-side encryption, so the SeaweedFS job — not this one — is
# where real SSE is exercised.
set -euo pipefail
kubectl create namespace s3mock --dry-run=client -o yaml | kubectl apply -f -
cat <<'EOF' | kubectl -n s3mock apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: { name: s3mock }
spec:
  replicas: 1
  selector: { matchLabels: { app: s3mock } }
  template:
    metadata: { labels: { app: s3mock } }
    spec:
      containers:
        - name: s3mock
          image: docker.io/adobe/s3mock:4.7.0
          env:
            - { name: initialBuckets, value: "stageset-rollback" }
          ports: [{ containerPort: 9090 }]
          readinessProbe:
            tcpSocket: { port: 9090 }
            initialDelaySeconds: 2
            periodSeconds: 3
---
apiVersion: v1
kind: Service
metadata: { name: s3mock }
spec:
  selector: { app: s3mock }
  ports: [{ port: 9090, targetPort: 9090 }]
EOF
kubectl -n s3mock rollout status deploy/s3mock --timeout=180s
