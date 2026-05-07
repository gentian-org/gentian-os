GOPATH ?= $(HOME)/go
LOCALBIN ?= $(GOPATH)/bin
export PATH := $(PATH):/usr/local/go/bin:$(LOCALBIN)

SHELL := /bin/bash

CONTROLLER_GEN ?= controller-gen
KUBECONFORM ?= kubeconform

CRD_OPTIONS ?= crd
BOILERPLATE := hack/boilerplate.go.txt

IMG ?= ghcr.io/gentian-org/gentian-os:latest

# Envtest binaries — set KUBEBUILDER_ASSETS to override (e.g. in CI via setup-envtest)
KUBEBUILDER_ASSETS ?= /tmp/envtest-bins/k8s/1.32.0-linux-amd64
export KUBEBUILDER_ASSETS

.PHONY: all build generate manifests test lint docker-build clean

all: generate build test

## Build the module (no binary yet — orchestrator binary added in Increment 2)
build:
	go build ./...

## Run unit tests
# internal/controller uses envtest whose watch goroutines conflict with -race;
# all other packages are tested with the race detector enabled.
test:
	go test $$(go list ./... | grep -v 'internal/controller') -race
	go test ./internal/controller/...

## Generate deepcopy methods
generate:
	$(CONTROLLER_GEN) object:headerFile="$(BOILERPLATE)" paths="./api/..."

## Generate CRD manifests
manifests:
	$(CONTROLLER_GEN) $(CRD_OPTIONS) paths="./api/..." output:crd:artifacts:config=config/crd

## Both generate and manifests in order
gen-all: generate manifests

## Verify generated files are up to date (CI check)
verify-gen: gen-all
	git diff --exit-code api/ config/crd/ || (echo "Generated files are out of date. Run 'make gen-all'." && exit 1)

## Tidy module dependencies
tidy:
	go mod tidy

## Run golangci-lint (install from https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

## Build the operator container image
docker-build:
	docker build -t $(IMG) .

clean:
	rm -rf config/crd/*.yaml api/v1alpha1/zz_generated.deepcopy.go

# ---------------------------------------------------------------------------
# Crossplane unit tests (no cluster required)
# ---------------------------------------------------------------------------

## Run crossplane render golden-file tests for all test cases in crossplane/tests/unit/render/
## Skip directories without an expected.yaml (run 'make test-unit-render-update' to generate them)
test-unit-render:
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
		actual=$$(crossplane render "$$dir/xr.yaml" "$$dir/composition.yaml" "$$dir/functions.yaml" 2>&1); \
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
		crossplane render "$$dir/xr.yaml" "$$dir/composition.yaml" "$$dir/functions.yaml" \
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
		crossplane beta validate crossplane/xrds/ "$$f" 2>&1 && echo "PASS: $$f" || { echo "FAIL: $$f"; exit 1; }; \
	done
	@echo "--- invalid fixtures (must fail validation)"
	@for f in crossplane/tests/unit/schema/invalid/*.yaml; do \
		[ -f "$$f" ] || continue; \
		if crossplane beta validate crossplane/xrds/ "$$f" 2>&1; then \
			echo "FAIL (expected rejection): $$f"; exit 1; \
		else \
			echo "PASS (correctly rejected): $$f"; \
		fi; \
	done

## Run all crossplane unit tests (render + functions + schema)
test-unit: test-unit-render test-unit-functions test-unit-schema
	@echo "=== all crossplane unit tests passed ==="

# ---------------------------------------------------------------------------
# E2E tests (live dev cluster required)
# ---------------------------------------------------------------------------

## Install crossplane CLI (if not present) and kubeconform (if not present)
install-tools:
	@which crossplane >/dev/null 2>&1 || { \
		echo "Installing crossplane CLI..."; \
		curl -sL https://raw.githubusercontent.com/crossplane/crossplane/master/install.sh | sh; \
		sudo mv crossplane /usr/local/bin/crossplane; \
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

## P1 — Kernel provisioning via Cluster XR (dev only) — not yet implemented
e2e-p1:
	@crossplane/tests/e2e/scripts/p1-kernel-dev.sh

## P2 — Migrate Pattern B charts to provider-helm (dev only) — not yet implemented
e2e-p2:
	@crossplane/tests/e2e/scripts/p2-pattern-b.sh

## P3 — Tenant XRD shadow deployment (dev only) — not yet implemented
e2e-p3:
	@crossplane/tests/e2e/scripts/p3-tenant-shadow.sh

## P4 — Cutover of a real tenant (dev only) — not yet implemented
e2e-p4:
	@crossplane/tests/e2e/scripts/p4-tenant-cutover.sh

## P5 — Migrate all tenants and decommission legacy stack — not yet implemented
e2e-p5:
	@crossplane/tests/e2e/scripts/p5-tofu-decommission.sh
