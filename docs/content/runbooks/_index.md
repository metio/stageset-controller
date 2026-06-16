---
title: Runbooks
---

One page per `status.conditions[Ready].reason` the controller sets, plus a few
operational alert runbooks. Each page covers the symptom, the cause, how to
diagnose it, and how to remediate.

Point the controller at a published copy of these pages with `--runbook-base-url`
(for example `https://stageset.projects.metio.wtf/runbooks`); the reason is then
appended to each actionable Ready message. Healthy reasons (`Succeeded`,
`Suspended`) get no link.
