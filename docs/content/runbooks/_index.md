---
title: Runbooks
---

One page per `status.conditions[Ready].reason` the controller sets, plus a few
operational alert runbooks. Each page covers the symptom, the cause, how to
diagnose it, and how to remediate.

The controller appends the matching page link to each actionable Ready message —
`(runbook: https://stageset.projects.metio.wtf/runbooks/<reason>/)` — so a
`kubectl describe` routes straight here. Healthy reasons (`Succeeded`,
`Suspended`) get no link.
