# Makefile for unvfd
#
# Conventions:
#   - All tools live in ./bin (NOT in $GOPATH/bin and NOT in /usr/local/bin).
#     The first thing every developer and CI does is `make tools`.
#   - The pinned tool versions below are the same versions CI uses. Do not
#     bump them in a feature commit; bump them in their own commit so the
#     toolchain delta is reviewable.
#   - `make lint` is the only authoritative lint entrypoint. It refuses to
#     run if the local golangci-lint is missing or its version does not
#     match the pin — the version drift between local and CI is the most
#     common source of "passes locally, fails in CI" lint bugs.
#
# Cross-compile note: `HOSTARCH` is detected from `uname -m`. Cross-builds
# (e.g. building an arm64 binary on an amd64 host) are done explicitly via
# `make build TARGET=linux/arm64`.

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

# Pinned tool versions. Bump deliberately, one per commit.
GOLANGCI_LINT_VERSION := v1.61.0
GOSEC_VERSION         := v2.21.4
GOVULNCHECK_VERSION   := v1.1.4
GITLEAKS_VERSION      := v8.21.2
TRUFFLEHOG_VERSION    := 3.84.1
SHELLCHECK_VERSION    := v0.10.0
SEMGREP_VERSION       := 1.95.0

# Project layout.
BIN_DIR      := bin
TOOLS_BIN    := $(BIN_DIR)/tools
LINT_BIN     := $(TOOLS_BIN)/golangci-lint
GOSEC_BIN    := $(TOOLS_BIN)/gosec
GOVULN_BIN   := $(TOOLS_BIN)/govulncheck
GITLEAKS_BIN := $(TOOLS_BIN)/gitleaks
TRUFFLE_BIN  := $(TOOLS_BIN)/trufflehog
SHCHK_BIN    := $(TOOLS_BIN)/shellcheck
SEMGREP_BIN  := $(TOOLS_BIN)/semgrep
BINARY       := $(BIN_DIR)/unvfd

# Build configuration.
GO          ?= go
GOFLAGS     ?= -trimpath
LDFLAGS     ?= -s -w -extldflags "-static"
BUILD_TAGS  ?=
CGO_ENABLED ?= 0

# Host architecture detection. Override with `make build TARGET=linux/arm64`
# for explicit cross-builds.
HOSTARCH := $(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
TARGET   ?= linux/$(HOSTARCH)
BIN_NAME := unvfd-$(TARGET)

# ---------------------------------------------------------------------------
# Phony targets
# ---------------------------------------------------------------------------

.PHONY: all help tools tools-check \
        build build-pie build-all \
        lint lint-fix lint-version \
        test test-race test-fuzz \
        vet vulncheck gosec secrets shellcheck semgrep \
        audit-logs ci ci-fast ci-slow \
        clean clean-tools clean-bin \
        format fmt-check

all: build

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Tool installation (local, pinned, idempotent)
# ---------------------------------------------------------------------------

tools: $(LINT_BIN) $(GOSEC_BIN) $(GOVULN_BIN) $(GITLEAKS_BIN) $(TRUFFLE_BIN) $(SHCHK_BIN) $(SEMGREP_BIN) ## Install all pinned tools into ./bin/tools

tools-check: ## Verify installed tools match the pinned versions
	@status=0; \
	for pair in \
	  '$(LINT_BIN):golangci-lint $(GOLANGCI_LINT_VERSION)' \
	  '$(GOSEC_BIN):gosec version $(GOSEC_VERSION)' \
	  '$(GOVULN_BIN):govulncheck' \
	  '$(GITLEAKS_BIN):gitleaks version $(GITLEAKS_VERSION)' \
	  '$(TRUFFLE_BIN):trufflehog $(TRUFFLEHOG_VERSION)' \
	  '$(SHCHK_BIN):shellcheck $(SHCHK_VERSION)' \
	  '$(SEMGREP_BIN):semgrep $(SEMGREP_VERSION)' ; do \
	  bin=$${pair%%:*}; expected=$${pair#*:}; \
	  if [ ! -x "$$bin" ]; then echo "MISSING: $$bin"; status=1; continue; fi; \
	  actual=$$("$$bin" --version 2>&1 | head -n1); \
	  case "$$actual" in \
	    *"$$expected"*) ;; \
	    *) echo "VERSION MISMATCH: $$bin"; echo "  expected: $$expected"; echo "  actual:   $$actual"; status=1 ;; \
	  esac; \
	done; \
	exit $$status

# golangci-lint uses the upstream installer. It auto-detects the host arch
# and OS, downloads the release, and verifies the SHA-256. The bin path
# is forced to ./bin/tools/golangci-lint via BINDIR.
$(LINT_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing golangci-lint $(GOLANGCI_LINT_VERSION) into $@"
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
	  | sh -s -- -b $(TOOLS_BIN) $(GOLANGCI_LINT_VERSION)
	@chmod +x $@

# govulncheck is a Go module, installed with `go install` to a pinned
# version. The `go.mod` and `go.sum` pin transitive deps; the version
# pin below is the user-facing one.
$(GOVULN_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing govulncheck $(GOVULNCHECK_VERSION) into $@"
	@GOBIN=$(CURDIR)/$(TOOLS_BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# gosec: a Go module, installed via `go install` to the pinned version.
$(GOSEC_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing gosec $(GOSEC_VERSION) into $@"
	@GOBIN=$(CURDIR)/$(TOOLS_BIN) $(GO) install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)

# gitleaks: pre-built binary release from GitHub.
$(GITLEAKS_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing gitleaks $(GITLEAKS_VERSION) into $@"
	@arch=$$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/'); \
	os=$$(uname | tr '[:upper:]' '[:lower:]'); \
	url="https://github.com/gitleaks/gitleaks/releases/download/$(GITLEAKS_VERSION)/gitleaks_$(GITLEAKS_VERSION:s/v//)_$${os}_$${arch}.tar.gz"; \
	echo "  fetching $$url"; \
	curl -sSfL "$$url" | tar -xz -C $(TOOLS_BIN) gitleaks; \
	chmod +x $@

# trufflehog: pre-built binary release from GitHub.
$(TRUFFLE_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing trufflehog $(TRUFFLEHOG_VERSION) into $@"
	@arch=$$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/'); \
	url="https://github.com/trufflesecurity/trufflehog/releases/download/v$(TRUFFLEHOG_VERSION:s/v//)/trufflehog_$(TRUFFLEHOG_VERSION)_linux_$${arch}.tar.gz"; \
	echo "  fetching $$url"; \
	curl -sSfL "$$url" | tar -xz -C $(TOOLS_BIN) trufflehog; \
	chmod +x $@

# shellcheck: pre-built binary release from GitHub.
$(SHCHK_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing shellcheck $(SHELLCHECK_VERSION) into $@"
	@arch=$$(uname -m | sed 's/x86_64/x86_64/; s/aarch64/aarch64/'); \
	url="https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/shellcheck-$(SHELLCHECK_VERSION:s/v//).$${arch}.tar.xz"; \
	echo "  fetching $$url"; \
	tmp=$$(mktemp -d); \
	curl -sSfL "$$url" | tar -xJ -C $$tmp; \
	cp $$tmp/shellcheck-$(SHELLCHECK_VERSION:s/v//)/shellcheck $@; \
	rm -rf $$tmp; \
	chmod +x $@

# semgrep: pip install — works in a venv or globally. We use the GitHub
# release tarball to avoid pulling pip into a Go project's deps.
$(SEMGREP_BIN):
	@mkdir -p $(TOOLS_BIN)
	@echo ">>> installing semgrep $(SEMGREP_VERSION) into $@"
	@arch=$$(uname -m | sed 's/x86_64/x86_64/; s/aarch64/aarch64/'); \
	url="https://github.com/semgrep/semgrep/releases/download/v$(SEMGREP_VERSION:s/v//)/semgrep_$(SEMGREP_VERSION)_$${arch}.tar.gz"; \
	echo "  fetching $$url"; \
	curl -sSfL "$$url" | tar -xz -C $(TOOLS_BIN); \
	mv $(TOOLS_BIN)/semgrep-$(SEMGREP_VERSION:s/v//) $(SEMGREP_BIN); \
	chmod +x $@

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build: ## Build unvfd for the host architecture (PIE, static, trimmed)
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build $(GOFLAGS) \
	  -buildmode=pie \
	  -ldflags '$(LDFLAGS)' \
	  -tags "$(BUILD_TAGS)" \
	  -o $(BINARY) ./cmd/unvfd

build-pie: ## Verify the produced binary is position-independent (ET_DYN)
	@out=$$($(GO) env GOEXE 2>/dev/null || true); \
	bin=$(BINARY)$$out; \
	if ! file "$$bin" | grep -q 'ELF .* (shared object|pie executable)'; then \
	  echo "FAIL: $$bin is not PIE (file output below)"; file "$$bin"; exit 1; \
	fi
	@echo "OK: $(BINARY) is PIE"

build-all: ## Cross-compile for amd64 and arm64
	@for tgt in linux/amd64 linux/arm64; do \
	  echo ">>> building for $$tgt"; \
	  CGO_ENABLED=0 GOOS=$${tgt%/*} GOARCH=$${tgt#*/} \
	    $(GO) build $(GOFLAGS) -buildmode=pie \
	      -ldflags '$(LDFLAGS)' \
	      -tags "$(BUILD_TAGS)" \
	      -o $(BIN_DIR)/$(BIN_NAME)-$${tgt##*/} ./cmd/unvfd; \
	done

# ---------------------------------------------------------------------------
# Test, lint, vet
# ---------------------------------------------------------------------------

test: ## Run unit tests with the race detector
	$(GO) test ./... -race -count=1 -shuffle=on

test-race: test ## alias

test-fuzz: $(LINT_BIN) ## Run native Go fuzz targets for 30s each (smoke)
	$(GO) test -fuzz=. -fuzztime=30s ./...

vet: ## go vet on all packages
	$(GO) vet ./...

# golangci-lint is the only authoritative lint command. It MUST come from
# the pinned ./bin/tools/golangci-lint. The `tools-check` guard at the
# top of the recipe is the safety net: if the binary is missing or its
# version does not match the pin, the developer (and CI) get a clear
# error pointing at `make tools` instead of an inconsistent build.
lint: tools-check $(LINT_BIN) ## Run the pinned golangci-lint (CI-equivalent)
	$(LINT_BIN) run --timeout 10m ./...

lint-fix: tools-check $(LINT_BIN) ## Run the pinned golangci-lint with --fix
	$(LINT_BIN) run --timeout 10m --fix ./...

lint-version: $(LINT_BIN) ## Print the pinned golangci-lint version
	$(LINT_BIN) --version

# ---------------------------------------------------------------------------
# Security scanning (slow layer; usually invoked from CI nightly)
# ---------------------------------------------------------------------------

vulncheck: $(GOVULN_BIN) ## govulncheck — reachable-call vulnerability scan
	$(GOVULN_BIN) ./...

gosec: $(GOSEC_BIN) ## gosec — Go security checker
	$(GOSEC_BIN) ./...

secrets: $(GITLEAKS_BIN) ## gitleaks — secret scanner
	$(GITLEAKS_BIN) detect --source . --no-git

shellcheck: $(SHCHK_BIN) ## shellcheck on scripts/
	@find scripts -type f -name '*.sh' -print0 | xargs -0 -n1 $(SHCHK_BIN)

semgrep: $(SEMGREP_BIN) ## semgrep — Go + OWASP + security-audit rulesets
	$(SEMGREP_BIN) --config p/security-audit --config p/owasp-top-ten --config p/golang .

# ---------------------------------------------------------------------------
# Custom in-tree checks
# ---------------------------------------------------------------------------

audit-logs: ## unvfd lint logs — verify no payload/destination in any logger call
	$(GO) run ./cmd/unvfd lint logs

# ---------------------------------------------------------------------------
# Aggregated CI entrypoints
# ---------------------------------------------------------------------------

ci-fast: format vet build build-pie lint vulncheck test ## Per-PR fast layer
	@echo ">>> fast CI: OK"

ci-slow: ci-fast gosec secrets shellcheck semgrep ## Full per-PR + nightly extras
	@echo ">>> slow CI: OK"

ci: ci-fast ## alias; CI runs `make ci`

# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------

format: ## gofmt + goimports over the tree
	$(GO) fmt ./...
	@which goimports >/dev/null 2>&1 && goimports -w . || echo "goimports not installed (run 'make tools' to fetch)"

fmt-check: ## Verify the tree is gofmt-clean (CI gate)
	@out=$$($(GO) fmt ./... 2>&1); \
	if [ -n "$$out" ]; then \
	  echo "gofmt found unformatted files:"; echo "$$out"; exit 1; \
	fi

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)/unvfd $(BIN_DIR)/unvfd-* $(BIN_DIR)/dist

clean-bin: clean ## alias

clean-tools: ## Remove locally installed tools
	rm -rf $(TOOLS_BIN)
