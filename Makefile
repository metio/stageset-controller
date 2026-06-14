# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.3
GOVULNCHECK    ?= go run golang.org/x/vuln/cmd/govulncheck@latest
GOFUMPT        ?= go run mvdan.cc/gofumpt@latest
STATICCHECK    ?= go run honnef.co/go/tools/cmd/staticcheck@latest
GOSEC          ?= go run github.com/securego/gosec/v2/cmd/gosec@latest
ARCH_GO        ?= go run github.com/arch-go/arch-go@latest

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: deps
deps: ## Resolve Go dependencies (run once after cloning the scaffold)
	go mod tidy

.PHONY: generate
generate: ## Generate DeepCopy implementations
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: ## Generate CRD manifests into config/crd
	$(CONTROLLER_GEN) crd rbac:roleName=stageset-controller paths="./..." output:crd:artifacts:config=config/crd

.PHONY: fmt
fmt: ## Format sources
	$(GOFUMPT) -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofumpt-formatted
	@test -z "$$($(GOFUMPT) -l .)" || { $(GOFUMPT) -l .; echo "run 'make fmt'"; exit 1; }

.PHONY: staticcheck
staticcheck: ## Run staticcheck with all checks enabled
	$(STATICCHECK) ./...

.PHONY: test
test: ## Run tests with race detector and shuffling
	go test -race -shuffle=on ./...

.PHONY: cover
cover: ## Run tests with coverage report
	go test -race -coverprofile=cover.out ./...
	go tool cover -html=cover.out -o cover.html

.PHONY: gosec
gosec: ## Run gosec security analyzer
	$(GOSEC) ./...

.PHONY: vuln
vuln: ## Run govulncheck
	$(GOVULNCHECK) ./...

.PHONY: arch
arch: ## Verify architecture rules (arch-go.yml)
	$(ARCH_GO)

.PHONY: reuse
reuse: ## Verify REUSE compliance (pipx install reuse)
	reuse lint

.PHONY: yamllint
yamllint: ## Lint YAML files (pipx install yamllint)
	yamllint .

.PHONY: build
build: ## Build the controller binary
	go build -o bin/stageset-controller ./cmd

.PHONY: docs
docs: ## Build the documentation site
	hugo --minify --source docs

.PHONY: docs-serve
docs-serve: ## Serve the documentation site locally
	hugo server --source docs

.PHONY: verify
verify: fmt-check vet staticcheck gosec test arch reuse yamllint ## Run every local check
