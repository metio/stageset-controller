# SPDX-FileCopyrightText: The stageset-controller Authors
# SPDX-License-Identifier: 0BSD

# Every correctness gate CI runs on a pull request, in one command.
#
# Assembling the list by hand before a push is how a gate gets skipped: leave
# one tool off and the omission is invisible until CI finds it. The recipe lives
# here so "the gate is green" means the same thing locally and in the merge
# queue.
#
# Mirrors metio/ci's reusable golang.yml (test / lint-go / vulnerabilities /
# architecture) plus this repo's own reuse, yaml, markdown and typos jobs. The
# text linters go through the shared ci-* wrappers, which pin the exact CI
# invocation, so those arguments are not spelled a second time here.
#
# The checks that need a browser, a registry, or a cluster are NOT here — the
# container scan, the kind smoke gate, and the docs-lint job's htmltest/biome
# steps stay in CI, and `website` builds the site if you want to check it.
#
# Runs every check before reporting, rather than stopping at the first failure:
# one pass should tell you everything to fix.

failed=""
step() {
  local name="$1"
  shift
  echo "== ${name}"
  if ! "$@"; then
    failed="${failed} ${name}"
  fi
}

# --- Go: metio/ci golang.yml ------------------------------------------------

step build go build ./...
# -shuffle=on because order-dependent tests are a real failure this catches; the
# devShell exports KUBEBUILDER_ASSETS, so the envtest-backed suites run here
# even though CI's Verify gate skips them.
step test go test -count=1 -race -shuffle=on ./...
step vet go vet ./...
step staticcheck staticcheck ./...
step gosec gosec -quiet ./...

echo "== gofumpt"
unformatted="$(gofumpt -l .)"
if [ -n "${unformatted}" ]; then
  echo "these files are not gofumpt-formatted:"
  echo "${unformatted}"
  failed="${failed} gofumpt"
fi

# controller-gen owns zz_generated.deepcopy.go, so its maps.Copy suggestions are
# not actionable; the same exclusion CI applies.
echo "== modernize"
findings="$(modernize ./... 2>&1 | grep -vE '^go: |zz_generated\.deepcopy\.go|^exit status ' || true)"
if [ -n "${findings}" ]; then
  echo "modernize found code that should adopt newer-Go idioms:"
  echo "${findings}"
  failed="${failed} modernize"
fi

step govulncheck govulncheck ./...
step arch-go arch-go

# --- Text, prose, licensing -------------------------------------------------

step reuse ci-reuse
step yaml ci-yaml
step actionlint ci-actionlint
step typos ci-typos

# The markdown the checkout TRACKS, which is what CI's `**/*.md` amounts to
# there: a fresh checkout has no untracked files and no submodule content. On a
# working clone that glob also reaches into docs/themes/metio — a separate repo
# with its own gate — and into anything git ignores, so it reports findings in
# files nobody here can fix. A gate that only passes on a machine with nothing
# checked out is a gate nobody runs before pushing.
echo "== markdown"
if ! git ls-files -z -- '*.md' '*.markdown' | xargs -0 -r markdownlint-cli2; then
  failed="${failed} markdown"
fi

# ----------------------------------------------------------------------------

if [ -n "${failed}" ]; then
  echo
  echo "FAILED:${failed}"
  exit 1
fi
echo
echo "all gates passed"
