# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD

# Regenerates every controller-gen artifact this repo commits:
#   - api/v1/zz_generated.deepcopy.go — DeepCopy implementations
#   - config/crd/*.yaml               — the CustomResourceDefinitions
#   - config/rbac/role.yaml           — the ClusterRole from the +kubebuilder:rbac markers
#   - config/webhook/manifests.yaml   — the webhook configuration
#
# Run it after any change to api/ or to a +kubebuilder:rbac marker, and commit
# the result. The `generated` job in verify.yml runs this same command and fails
# on a diff, so the gate cannot disagree with the recipe.
#
# headerFile is REQUIRED on the object generator: zz_generated.deepcopy.go
# carries an inline SPDX header, and controller-gen rewrites the file wholesale —
# without it the header is dropped and REUSE fails.

go tool controller-gen crd rbac:roleName=stageset-controller webhook paths="./..." output:crd:artifacts:config=config/crd output:webhook:artifacts:config=config/webhook
go tool controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
