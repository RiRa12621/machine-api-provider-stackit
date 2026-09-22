GO ?= go
GOLANGCI_LINT ?= golangci-lint
ENVTEST_VERSION ?= 1.34.1
SETUP_ENVTEST_VERSION := v0.0.0-20240923090159-236e448db12c
ENVTEST_DIR := $(CURDIR)/.envtest
GO_TEST_FLAGS ?= -race -count=1 -timeout=15m

.PHONY: build test test-envtest setup-envtest fmt verify-fmt vet lint check
build:
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -o bin/machine-controller-manager ./cmd/machine-controller-manager

test:
	$(GO) test -mod=readonly $(GO_TEST_FLAGS) ./...

setup-envtest:
	$(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_VERSION) --bin-dir $(ENVTEST_DIR) -p path

test-envtest:
	@test -n "$(KUBEBUILDER_ASSETS)" || { echo 'Set KUBEBUILDER_ASSETS to the path printed by make setup-envtest'; exit 1; }
	$(GO) test -mod=readonly $(GO_TEST_FLAGS) -tags=envtest ./test/envtest

fmt:
	gofmt -w api cmd pkg test

verify-fmt:
	@test -z "$$(gofmt -l api cmd pkg test)" || { echo 'Run make fmt'; exit 1; }

vet:
	$(GO) vet -mod=readonly ./...

lint:
	$(GOLANGCI_LINT) run --modules-download-mode=readonly --timeout=10m

check: verify-fmt vet lint test build
