---
title: Runbooks
---

Start from the `status.conditions[Ready].reason` on a StageSet, or from a firing
operational alert, and follow the matching page to diagnose and remediate the
symptom.

The controller appends the matching page link to each actionable Ready message —
`(runbook: https://stageset.projects.metio.wtf/runbooks/<reason>/)` — so a
`kubectl describe` routes you straight here. Healthy reasons (`Succeeded`,
`Suspended`) carry no link.
