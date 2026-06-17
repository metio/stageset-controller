---
title: Webhook cert renewal failing
description: The self-signed admission webhook certificate is not being rotated.
tags: [runbooks, security, operations, troubleshooting]
---

## Symptom

`stageset_webhook_cert_renewal_failures_total` is increasing; the
`StageSetWebhookCertRenewalFailing` alert fires (see
[operations](/installation/operations/) for the alert set and its thresholds).
The current certificate keeps working until its natural expiry — that expiry is
the deadline, after which cluster-wide `StageSet` admission breaks.

## Cause

Only applies in `--webhook-cert-mode=self-signed`. The in-pod renewer regenerates
the serving cert every `validity/3` and patches the
`ValidatingWebhookConfiguration`'s `caBundle`. It fails when:

- the controller lost `update` (or `get`) on the named
  `ValidatingWebhookConfiguration` (`--webhook-validating-config-name`),
- the VWC was renamed and the flag/`resourceNames` weren't updated,
- the cert directory (`--webhook-cert-dir`) became read-only.

In `cert-manager` mode this metric is irrelevant — [cert-manager](https://cert-manager.io/) owns renewal.

## Diagnosis

```shell
kubectl --namespace stageset-system logs deploy/stageset-controller | grep -i 'cert\|renew\|caBundle'
kubectl get validatingwebhookconfiguration <name> --output jsonpath='{.webhooks[*].clientConfig.caBundle}' | head -c 40
```

## Remediation

- Restore `get`/`update` on the named VWC in the controller's ClusterRole
  (`resourceNames` must include it).
- Fix the `--webhook-validating-config-name` / `--webhook-cert-dir` flags if they
  drifted from the deployed VWC and mount.
- As a longer-term option, switch to `--webhook-cert-mode=cert-manager` so renewal
  is handled by cert-manager.
