# CRDs

CRD manifests are generated from the Go types, not hand-written:

```sh
nix develop --command generate
```

This runs `controller-gen` over `api/` and writes the CustomResourceDefinitions
for `StageSet` and `StageInventory` into this directory, alongside
`config/rbac/role.yaml` (from the `+kubebuilder:rbac` markers) and
`config/webhook/manifests.yaml`. Commit whatever it produces: the `generated`
job in `verify.yml` runs the same command and fails on a diff.

The command is declared in `flake.nix` and reads `scripts/generate.sh` — the one
definition CI and contributors share. `make generate` calls the same script.

Remember to add the `applyset.kubernetes.io/is-parent-type: "true"` label to the
StageInventory CRD (a kustomize patch in this directory is the right place once
the base is generated).
