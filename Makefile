GOPATH ?= $(HOME)/go
LOCALBIN ?= $(GOPATH)/bin
export PATH := $(PATH):/usr/local/go/bin:$(LOCALBIN)

SHELL := /bin/bash

CONTROLLER_GEN ?= controller-gen
KUBECONFORM ?= kubeconform

CRD_OPTIONS ?= crd
BOILERPLATE := hack/boilerplate.go.txt

IMG ?= ghcr.io/gentian-org/gentian-os:latest

# Crossplane CLI/core version used for install-tools, CI, and schema validation.
# Read from versions.yaml so the pin exists once — it was previously duplicated
# here, in install.sh and in scripts/bootstrap/install-crossplane-cli.sh, in two spellings.
CROSSPLANE_CLI_VERSION ?= $(shell bash scripts/lib/versions.sh crossplane cli)
CROSSPLANE_IMAGE ?= xpkg.crossplane.io/crossplane/crossplane:$(CROSSPLANE_CLI_VERSION)

# Envtest binaries — set KUBEBUILDER_ASSETS to override (e.g. in CI via setup-envtest)
KUBEBUILDER_ASSETS ?= /tmp/envtest-bins/k8s/1.32.0-linux-amd64
export KUBEBUILDER_ASSETS

.PHONY: all build generate manifests test lint docker-build clean install-plugin validate-steps gen-credentials gen-phase-table lint-phase-table check-credentials lint-cluster-config-keys lint-template-placeholders lint-portability lint-image-digests check-render-fixtures lint-resolvable lint-step-contracts lint-claim-defaults lint-password-schemes test-policy test-policy-openbao test-policy-authz verify-claim-applied

all: generate build test

## Build the module (operator manager)
build:
	go build -o bin/manager ./cmd/main.go

## Install the kubectl-gentian plugin and gtnctl symlink to ~/.local/bin
install-plugin:
	install -d $(HOME)/.local/bin
	install -m 0755 scripts/kubectl-gentian $(HOME)/.local/bin/kubectl-gentian
	ln -sf kubectl-gentian $(HOME)/.local/bin/gtnctl
	@echo "Installed kubectl-gentian and gtnctl (-> kubectl-gentian) to $(HOME)/.local/bin"

## Run unit tests
# internal/controller uses envtest whose watch goroutines conflict with -race;
# all other packages are tested with the race detector enabled.
test:
	go test $$(go list ./... | grep -v 'internal/controller') -race
	# -timeout, because envtest waits are bounded at 3 minutes each: enough
	# simultaneous failures would exceed Go's 10-minute default and replace
	# readable per-test failures with a whole-package panic dump.
	go test ./internal/controller/... -timeout 20m

## Generate deepcopy methods
generate:
	$(CONTROLLER_GEN) object:headerFile="$(BOILERPLATE)" paths="./api/..."

## Generate CRD manifests and sync them into the Helm chart crds/ directory
manifests:
	$(CONTROLLER_GEN) $(CRD_OPTIONS) paths="./api/..." output:crd:artifacts:config=config/crd
	@# Not a blanket copy: Apps and XTenants are Crossplane XRD-generated, and
	@# Helm claims server-side apply ownership of everything in crds/, so shipping
	@# them there fights Crossplane for the same objects — see crds/README.md.
	@# The blanket copy used to drop both into crds/ on every run, leaving two
	@# untracked files one commit away from breaking the install.
	@for f in config/crd/gentianos.io_*.yaml; do \
		case "$$f" in *_apps.yaml|*_xtenants.yaml) continue ;; esac; \
		cp "$$f" charts/gentian-os/crds/; \
	done
	@# RBAC is generated from the +kubebuilder:rbac markers, which live in
	@# ./internal/... — NOT ./api/..., where the CRD run above looks. Scanning
	@# only ./api/... is what let the chart's hand-written ClusterRole drift from
	@# the markers for eleven separate permissions; see scripts/gen/gen-clusterrole.py.
	$(CONTROLLER_GEN) rbac:roleName=gentian-os paths="./internal/..." output:rbac:artifacts:config=config/rbac
	python3 scripts/gen/gen-clusterrole.py

## Render the Keycloak login theme sources into the ConfigMap Argo CD applies
gen-theme:
	python3 scripts/gen/gen-keycloak-theme-configmap.py

## Render credentials.yaml into the packaged CredentialRequirement CRs
gen-credentials:
	python3 scripts/gen/gen-credential-requirements.py

## Both generate and manifests in order
gen-all: generate manifests gen-theme gen-credentials

## Verify generated files are up to date (CI check)
verify-gen: gen-all
	python3 scripts/gen/gen-credential-requirements.py --check
	git diff --exit-code api/ config/crd/ charts/gentian-os/crds/ charts/gentian-os/templates/clusterrole.yaml kernel/services/keycloak-idp/manifests/ kernel/credentials/ || (echo "Generated files are out of date. Run 'make gen-all'." && exit 1)

## Tidy module dependencies
tidy:
	go mod tidy

## Run every linter the CI Lint job runs (Go, YAML, shell)
lint: lint-go lint-yaml lint-shell

## Run golangci-lint (install from https://golangci-lint.run/usage/install/)
lint-go:
	golangci-lint run ./...

## Run yamllint over the repo, as .github/workflows/ci.yaml does
lint-yaml:
	yamllint -c .yamllint.yml .

## Run shellcheck over every tracked shell script, as .github/workflows/ci.yaml does.
## The file list and flags must match CI exactly: -x follows sourced files, and no
## -S filter means info/style findings fail the build too. Hand-rolling a narrower
## invocation is how an SC2153 reached develop green-looking.
lint-shell: validate-steps lint-step-contracts lint-resolvable lint-credential-fields lint-claim-defaults lint-cluster-config-keys lint-template-placeholders lint-phase-table lint-password-schemes
	@git ls-files -z -- '*.sh' | xargs -0 shellcheck -x scripts/kubectl-gentian

## Report which declared credentials are satisfied. --source picks where to look:
## vault (installer preflight), cluster (day-2), git (CI on a deployments branch).
check-credentials:
	@bash scripts/check-credentials.sh --source=$${SOURCE:-cluster}

## Assert every pinned image digest is a manifest list, not a single
## architecture. Queries the registry, so it needs network and is not part of
## the offline lint set.
lint-image-digests:
	@bash scripts/lint/lint-image-digests.sh

## Assert every function call in every shell file resolves. Catches deleting a
## function whose last caller was not checked — the most repeated mistake here.
lint-step-contracts:
	@bash scripts/lint/lint-step-contracts.sh

lint-resolvable:
	@bash scripts/lint/lint-resolvable.sh

## Assert every reader of an OpenBao path names a field the catalogue declares
## and the installer writes. A value under the right path with the wrong key
## reads as absent and presents as an ESO fault; it has cost two clusters.
lint-credential-fields:
	@python3 scripts/lint/lint-credential-fields.py

## Report shell defaults for settings the Cluster XRD already answers. Expected
## non-zero until the call sites read the claim; the number must only go down.
## Fail if anything writes a password where Dovecot expects a hash.
lint-password-schemes:
	@bash scripts/lint/lint-password-schemes.sh

lint-claim-defaults:
	@bash scripts/lint/lint-claim-defaults.sh

## Assert every gentian-cluster-config key a Composition reads is one the
## producer writes. sprig's dig defaults a missing key to "", so a rename or
## typo renders empty rather than failing — and the render fixtures do not
## catch it either, because they supply a partial ConfigMap.
lint-cluster-config-keys:
	@python3 scripts/lint/lint-cluster-config-keys.py

## Regenerate the §11 phase table from the phase sections
gen-phase-table:
	@python3 scripts/gen/gen-phase-table.py

## Fail when the phase table disagrees with the phase sections
lint-phase-table:
	@python3 scripts/gen/gen-phase-table.py --check

## Shell placeholders in Helm templates, which nothing expands
lint-template-placeholders:
	@python3 scripts/lint/lint-template-placeholders.py

## Report macOS/BSD portability violations. Expected non-zero until Phase 13
## migrates the call sites; the count must only go down.
lint-portability:
	@bash scripts/lint/lint-portability.sh

## Assert every scripts/steps/*.sh declares its contract and defines apply().
## Reads only the step files — no cluster, no kubeconfig.
validate-steps:
	@SCRIPT_DIR="$(CURDIR)" bash -c 'source scripts/lib/load.sh; source scripts/lib/driver.sh; validate_steps'

## Build the operator container image
docker-build:
	docker build -t $(IMG) .

clean:
	rm -rf config/crd/*.yaml api/v1alpha1/zz_generated.deepcopy.go

# ---------------------------------------------------------------------------
# Crossplane unit tests (no cluster required)
# ---------------------------------------------------------------------------

## Assert each render test's Composition copy matches the deployed one. A stale
## copy keeps the golden test green against a Composition nobody runs.
check-render-fixtures:
	@bash scripts/lint/check-render-fixtures.sh

## Run crossplane render golden-file tests for all test cases in crossplane/tests/unit/render/
## Skip directories without an expected.yaml (run 'make test-unit-render-update' to generate them)
test-unit-render: check-render-fixtures
	@echo "=== crossplane render golden tests ==="
	@failed=0; \
	for dir in crossplane/tests/unit/render/*/; do \
		name=$$(basename "$$dir"); \
		if [ ! -f "$$dir/xr.yaml" ] || [ ! -f "$$dir/composition.yaml" ] || [ ! -f "$$dir/functions.yaml" ]; then \
			echo "SKIP: $$name (missing xr/composition/functions.yaml)"; \
			continue; \
		fi; \
		if [ ! -f "$$dir/expected.yaml" ]; then \
			echo "SKIP: $$name (no expected.yaml — run 'make test-unit-render-update' to generate)"; \
			continue; \
		fi; \
		req_args=""; \
		if [ -d "$$dir/required-resources" ]; then \
			req_args="-e $$dir/required-resources"; \
		fi; \
		obs_args=""; \
		if [ -d "$$dir/observed-resources" ]; then \
			obs_args="-o $$dir/observed-resources"; \
		elif [ -f "$$dir/observed-resources.yaml" ]; then \
			obs_args="-o $$dir/observed-resources.yaml"; \
		fi; \
		actual=$$(crossplane render "$$dir/xr.yaml" "$$dir/composition.yaml" "$$dir/functions.yaml" $$req_args $$obs_args 2>&1); \
		rc=$$?; \
		if [ $$rc -ne 0 ]; then \
			echo "FAIL: $$name (crossplane render exited $$rc)"; \
			echo "$$actual"; \
			failed=1; \
		else \
			diff_out=$$(diff -u "$$dir/expected.yaml" <(echo "$$actual")); \
			if [ -n "$$diff_out" ]; then \
				echo "FAIL: $$name (output differs from expected.yaml)"; \
				echo "$$diff_out"; \
				failed=1; \
			else \
				echo "PASS: $$name"; \
			fi; \
		fi; \
	done; \
	exit $$failed

## Regenerate all expected.yaml golden files under crossplane/tests/unit/render/
test-unit-render-update:
	@echo "=== regenerating render golden files ==="
	@for dir in crossplane/tests/unit/render/*/; do \
		name=$$(basename "$$dir"); \
		if [ ! -f "$$dir/xr.yaml" ] || [ ! -f "$$dir/composition.yaml" ] || [ ! -f "$$dir/functions.yaml" ]; then \
			echo "SKIP: $$name (missing xr/composition/functions.yaml)"; \
			continue; \
		fi; \
		req_args=""; \
		if [ -d "$$dir/required-resources" ]; then \
			req_args="-e $$dir/required-resources"; \
		fi; \
		obs_args=""; \
		if [ -d "$$dir/observed-resources" ]; then \
			obs_args="-o $$dir/observed-resources"; \
		elif [ -f "$$dir/observed-resources.yaml" ]; then \
			obs_args="-o $$dir/observed-resources.yaml"; \
		fi; \
		crossplane render "$$dir/xr.yaml" "$$dir/composition.yaml" "$$dir/functions.yaml" $$req_args $$obs_args \
			> "$$dir/expected.yaml" && echo "UPDATED: $$name" || echo "FAIL: $$name"; \
	done

## Run language-native function unit tests
test-unit-functions:
	@echo "=== function unit tests ==="
	@PY_EXIT=0; \
	if find crossplane/tests/unit/functions -name 'test_*.py' 2>/dev/null | grep -q .; then \
		python3 -m pytest crossplane/tests/unit/functions/ -v; \
		PY_EXIT=$$?; \
	else \
		echo "SKIP: no Python function tests found"; \
	fi; \
	if find crossplane/tests/unit/functions -name '*_test.go' 2>/dev/null | grep -q .; then \
		find crossplane/tests/unit/functions -name '*_test.go' | while read f; do \
			go test "$$(dirname $$f)/..."; \
		done; \
	fi; \
	exit $$PY_EXIT

## Run XRD schema validation tests using crossplane beta validate
test-unit-schema:
	@echo "=== XRD schema tests ==="
	@echo "--- valid fixtures (must pass)"
	@for f in crossplane/tests/unit/schema/valid/*.yaml; do \
		[ -f "$$f" ] || continue; \
		crossplane beta validate crossplane/xrds/ "$$f" \
			--crossplane-image="$(CROSSPLANE_IMAGE)" 2>&1 \
			&& echo "PASS: $$f" || { echo "FAIL: $$f"; exit 1; }; \
	done
	@echo "--- invalid fixtures (must fail validation)"
	@for f in crossplane/tests/unit/schema/invalid/*.yaml; do \
		[ -f "$$f" ] || continue; \
		if crossplane beta validate crossplane/xrds/ "$$f" \
			--crossplane-image="$(CROSSPLANE_IMAGE)" 2>&1; then \
			echo "FAIL (expected rejection): $$f"; exit 1; \
		else \
			echo "PASS (correctly rejected): $$f"; \
		fi; \
	done

## Run all crossplane unit tests (render + functions + schema)
test-unit: test-unit-render test-unit-functions test-unit-schema
	@echo "=== all crossplane unit tests passed ==="

## Assert what the authorization rules actually permit, not just what they say.
## Two layers that fail in different ways:
##   OpenBao path policies — who may read which secret path. The tenant-admin
##   deny on gentian-os/kernel/* is the one rule where a mistake is a breach.
##   Skips cleanly when `bao` is absent.
##   The OpenFGA model — who may launch an app or read a document.
## test-unit-render covers the same policies as TEXT; these run them.
test-policy: test-policy-openbao test-policy-authz
	@echo "=== authorization policy tests passed ==="

test-policy-openbao:
	@bash scripts/tools/verify-openbao-policies.sh

test-policy-authz:
	@bash scripts/tools/verify-authz-model.sh

## verify-claim-applied: the live cluster carries what its Cluster claim says.
##
## Not part of `test`, because it needs a cluster rather than a checkout. The
## settings that reach ApplicationSets do so as Helm parameters the installer
## writes once and never re-applies, so git and the claim can agree while the
## cluster runs on something else — which is how mail.serviceMode came to say
## kernel in the claim and external on the cluster, leaving Dovecot running
## with no owner. Read-only: it reports, it does not reconcile.
verify-claim-applied:
	@bash scripts/tools/verify-claim-applied.sh

# ---------------------------------------------------------------------------
# E2E tests (live dev cluster required)
# ---------------------------------------------------------------------------

## Install crossplane CLI (if not present) and kubeconform (if not present)
install-tools:
	@which crossplane >/dev/null 2>&1 || { \
		echo "Installing crossplane CLI..."; \
		tmpdir=$$(mktemp -d); \
		( cd "$$tmpdir" && XP_VERSION=$(CROSSPLANE_CLI_VERSION) bash "$(shell pwd)/scripts/bootstrap/install-crossplane-cli.sh" \
		  && sudo mv crossplane /usr/local/bin/crossplane ); \
		rmdir "$$tmpdir"; \
	}
	@which kubeconform >/dev/null 2>&1 || { \
		echo "Installing kubeconform..."; \
		KUBECONFORM_VERSION=v0.6.7; \
		OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
		ARCH=$$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/'); \
		curl -sL "https://github.com/yannh/kubeconform/releases/download/$${KUBECONFORM_VERSION}/kubeconform-$${OS}-$${ARCH}.tar.gz" \
			| sudo tar -xz -C /usr/local/bin kubeconform; \
	}
	@echo "crossplane: $$(crossplane version 2>&1 | head -1)"
	@echo "kubeconform: $$(kubeconform -v)"

## P0 — Install Crossplane core on the dev cluster and verify it is Ready
e2e-p0:
	@crossplane/tests/e2e/scripts/p0-crossplane-install.sh

## P0 rollback — uninstall Crossplane core from the dev cluster
e2e-p0-clean:
	@echo "Uninstalling Crossplane core..."
	helm uninstall crossplane -n crossplane-system 2>/dev/null || true
	kubectl delete ns crossplane-system --ignore-not-found=true
	kubectl delete clusterrole crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse --ignore-not-found=true
	kubectl delete clusterrolebinding crossplane crossplane-admin crossplane-edit crossplane-view crossplane-browse --ignore-not-found=true
	@echo "Done."

## P1 — Kernel provisioning via Cluster XR (dev cluster)
e2e-p1:
	@crossplane/tests/e2e/scripts/p1-kernel-dev.sh

## P2 — Pattern B kernel Helm Releases (dev cluster)
e2e-p2:
	@crossplane/tests/e2e/scripts/p2-pattern-b.sh

## P3 — Tenant shadow deployment (Crossplane graph verification)
e2e-p3:
	@crossplane/tests/e2e/scripts/p3-tenant-shadow.sh

## P4 — Cutover verification for an existing tenant
e2e-p4:
	@crossplane/tests/e2e/scripts/p4-tenant-cutover.sh

## P5 — Keycloak + Dovecot install smoke (requires Stage 1 install on cluster)
e2e-p5-keycloak-dovecot:
	@crossplane/tests/e2e/scripts/e2e-verify-kernel-services.sh

## Re-run kernel service smoke checks without full install
verify-kernel-services:
	@crossplane/tests/e2e/scripts/e2e-verify-kernel-services.sh
