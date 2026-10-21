SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

MODULE   := github.com/jstudler/greenlight
BINARY   := greenlight
IMAGE    ?= ghcr.io/jstudler/greenlight
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X $(MODULE)/internal/app.Version=$(VERSION)

BIN_DIR        := $(CURDIR)/bin
ENVTEST        := $(BIN_DIR)/setup-envtest
ENVTEST_K8S    ?= 1.34.x
GOLANGCI_LINT  := $(BIN_DIR)/golangci-lint
KIND_CLUSTER   ?= kind

GOLANGCI_LINT_VERSION   ?= v2.15.0
SETUP_ENVTEST_VERSION   ?= release-0.25

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make <target>\n"} \
	/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/greenlight

.PHONY: install
install: ## Install the binary into GOBIN
	go install -trimpath -ldflags="$(LDFLAGS)" ./cmd/greenlight

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

##@ Test

.PHONY: test
test: ## Run the unit tests
	go test ./internal/... ./cmd/...

.PHONY: test-integration
test-integration: envtest-assets ## Run the envtest integration tests
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S) -p path)" \
		go test ./test/integration/... -timeout 15m

.PHONY: test-all
test-all: test test-integration ## Run every test

.PHONY: cover
cover: envtest-assets ## Run every test and write cover.html
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S) -p path)" \
		go test ./... -coverprofile=cover.out -covermode=atomic -timeout 15m
	go tool cover -html=cover.out -o cover.html

.PHONY: envtest-assets
envtest-assets: $(ENVTEST) ## Download the envtest control plane binaries
	@$(ENVTEST) use $(ENVTEST_K8S) -p path >/dev/null

$(ENVTEST):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

##@ Quality

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: $(GOLANGCI_LINT) ## Run golangci-lint with --fix
	$(GOLANGCI_LINT) run --fix

.PHONY: fmt
fmt: ## Format the code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

$(GOLANGCI_LINT):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Rules

.PHONY: sync-rules
sync-rules: ## Copy examples/ into config/manager/rules/ (kustomize cannot read outside its own directory)
	cp examples/netbox.rules.yaml examples/generic.rules.yaml config/manager/rules/

.PHONY: verify-rules
verify-rules: sync-rules ## Fail if config/manager/rules/ has drifted from examples/
	@git diff --exit-code -- config/manager/rules || 		(echo "config/manager/rules is out of date, run: make sync-rules" && exit 1)

.PHONY: validate-rules
validate-rules: build ## Compile the shipped example rules without contacting a cluster
	KUBECONFIG=/nonexistent $(BIN_DIR)/$(BINARY) --rules examples/netbox.rules.yaml --validate
	KUBECONFIG=/nonexistent $(BIN_DIR)/$(BINARY) --rules examples/generic.rules.yaml --validate

.PHONY: validate-examples
validate-examples: build ## Resolve the example rules against the current cluster
	$(BIN_DIR)/$(BINARY) --rules examples/netbox.rules.yaml --validate
	$(BIN_DIR)/$(BINARY) --rules examples/generic.rules.yaml --validate

##@ Container and deployment

.PHONY: docker-build
docker-build: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

.PHONY: docker-push
docker-push: ## Push the container image
	docker push $(IMAGE):$(VERSION)

.PHONY: deploy-kind
deploy-kind: docker-build sync-rules ## Build, load into kind and apply the kustomization
	kind load docker-image $(IMAGE):$(VERSION) --name $(KIND_CLUSTER)
	cd config/manager && kustomize edit set image greenlight=$(IMAGE):$(VERSION)
	kubectl apply -k config/default

.PHONY: undeploy-kind
undeploy-kind: ## Remove greenlight from the kind cluster
	kubectl delete -k config/default --ignore-not-found

##@ Housekeeping

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BIN_DIR) dist cover.out cover.html
