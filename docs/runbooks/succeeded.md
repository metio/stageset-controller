# Reason: Succeeded

## Symptom

`READY=True`, `REASON=Succeeded`. The Message names the applied revisions.

## Meaning

This is the healthy steady state: every stage's artifact resolved, built, applied, and passed its readiness checks, and `status.lastAppliedRevisions` matches `status.lastAttemptedRevisions`. There is nothing to remediate.

## Notes

- The controller keeps reconciling at `spec.interval`; a re-render upstream (a new ExternalArtifact revision) re-applies automatically and the condition stays `Succeeded` once the new revision converges.
- `status.stages[]` reports per-stage `appliedRevision` and inventory entry counts if you want to confirm what each stage owns.
