---
title: Contributing
description: Build and test stageset-controller locally, pass the pull-request checks, and understand how releases are cut.
tags: [contributing, development, dco]
---

Contributions are welcome. Build and test the controller locally, satisfy the
checks every pull request must pass, and see how releases are cut.

## Developer Certificate of Origin

Every commit must be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/) — it
certifies you wrote the patch or otherwise have the right to contribute it. Add
the sign-off automatically with:

```shell
git commit --signoff
```

This appends a `Signed-off-by: Your Name <you@example.com>` line to the commit
message. The DCO check on each pull request enforces it; unsigned commits block
the merge. Amend an existing commit with `git commit --amend --signoff`.
