GOPATH ?= $(HOME)/go
LOCALBIN ?= $(GOPATH)/bin
export PATH := $(PATH):/usr/local/go/bin:$(LOCALBIN)

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
