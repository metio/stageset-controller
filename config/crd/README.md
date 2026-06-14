# CRDs

CRD manifests are generated from the Go types, not hand-written:

```sh
make manifests
```

This runs `controller-gen` over `api/v1` and writes the
CustomResourceDefinitions for `StageSet` and `StageInventory` into this
directory. Remember to add the
`applyset.kubernetes.io/is-parent-type: "true"` label to the StageInventory
CRD (a kustomize patch in this directory is the right place once the base is
generated).
