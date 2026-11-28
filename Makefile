# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

.PHONY: build test integration lint quality quality-deep proto clean docker fmt vet install-hooks services \
	access-catalog access-catalog-check \
	deploy-standalone deploy-kubernetes undeploy-standalone undeploy-kubernetes \
	install uninstall fresh-kubernetes-demo foundry-creds kind-load cert-manager \
	docker-k8s docker-standalone \
	samples-list samples-install samples-full-install samples-provision-azure samples-uninstall \
	status logs port-forward studio e2e e2e-kubernetes-memory e2e-kubernetes-audit e2e-kubernetes-tool e2e-kubernetes-tool-resilience e2e-kubernetes-tool-monitoring e2e-kubernetes-runtime-tool e2e-kubernetes-tool-network-policy e2e-kubernetes-tool-exec-probe e2e-kubernetes-model e2e-kubernetes-capability-rotation e2e-kubernetes-agent-communication quickstart \
	observability observability-install observability-uninstall jaeger jaeger-install jaeger-ui jaeger-uninstall grafana-ui

# Binary output directory
BIN_DIR := bin

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOVET := $(GOCMD) vet
GOFMT := gofmt
GOMOD := $(GOCMD) mod
GOLANGCI_LINT ?= golangci-lint
PYTHON ?= python3
TEST_TIMEOUT_SECONDS ?= 1800

# Service manager defaults (override with make variables)
ACTION ?= help
SERVICE ?=

# Deployment mode for `make install` (standalone|kubernetes)
MODE ?= standalone

# Binaries
APISERVER := $(BIN_DIR)/kognition-apiserver
CONTROLLER := $(BIN_DIR)/kognition-controller-manager
CTL := $(BIN_DIR)/kognitionctl
INTENT_GATEWAY := $(BIN_DIR)/kognition-intent-gateway
MEMORY_POLICY_GATEWAY := $(BIN_DIR)/kognition-memory-policy-gateway
TOOL_GATEWAY := $(BIN_DIR)/kognition-tool-gateway
MODEL_GATEWAY := $(BIN_DIR)/kognition-model-gateway
AGENT_COMMUNICATION_GATEWAY := $(BIN_DIR)/kognition-agent-communication-gateway
KNOWLEDGE_GATEWAY := $(BIN_DIR)/kognition-knowledge-gateway
KNOWLEDGE_AZURESEARCH := $(BIN_DIR)/kognition-knowledge-azuresearch

# Chart appVersion — images consumed by the Helm chart are also tagged with this
# so a fresh `make docker` is immediately picked up by `helm upgrade`.
APP_VERSION ?= $(shell awk '/^appVersion:/ {gsub(/"/,"",$$2); print $$2; exit}' deploy/helm/kognition/Chart.yaml)

# Helm / Kubernetes variables
NAMESPACE ?= kognition-system
AGENT_NAMESPACE ?= default
OBSERVABILITY_NAMESPACE ?= kognition-observability
RELEASE ?= kognition
CHART := deploy/helm/kognition
SAMPLE ?=
COMPONENT ?=
JAEGER_MANIFEST := deploy/observability/jaeger.yaml
PROMETHEUS_VALUES := deploy/observability/prometheus-values.yaml
PROMETHEUS_STACK_VERSION ?= 88.5.2
FRESH_IMAGE_TAG ?=
K8S_RUNTIME_IMAGE_TAG = $(if $(FRESH_IMAGE_TAG),$(FRESH_IMAGE_TAG),$(APP_VERSION))
K8S_FRESH_HELM_IMAGE_ARGS = $(if $(FRESH_IMAGE_TAG),--set k8sController.image.tag=$(FRESH_IMAGE_TAG) --set memoryService.image.tag=$(FRESH_IMAGE_TAG) --set memoryPolicyGateway.image.tag=$(FRESH_IMAGE_TAG) --set toolGateway.image.tag=$(FRESH_IMAGE_TAG) --set modelGateway.image.tag=$(FRESH_IMAGE_TAG) --set agentCommunicationGateway.image.tag=$(FRESH_IMAGE_TAG) --set intentGateway.image.tag=$(FRESH_IMAGE_TAG) --set kbIngester.image.tag=$(FRESH_IMAGE_TAG),)
# Default target
all: build

## build: Build all binaries
build: $(APISERVER) $(CONTROLLER) $(CTL) $(INTENT_GATEWAY) $(MEMORY_POLICY_GATEWAY) $(TOOL_GATEWAY) $(MODEL_GATEWAY) $(AGENT_COMMUNICATION_GATEWAY) $(KNOWLEDGE_GATEWAY) $(KNOWLEDGE_AZURESEARCH)

$(APISERVER):
	$(GOBUILD) -o $@ ./cmd/kognition-apiserver

$(CONTROLLER):
	$(GOBUILD) -o $@ ./cmd/kognition-controller-manager

$(CTL):
	$(GOBUILD) -o $@ ./cmd/kognitionctl

$(INTENT_GATEWAY):
	$(GOBUILD) -o $@ ./cmd/kognition-intent-gateway

$(MEMORY_POLICY_GATEWAY):
	$(GOBUILD) -o $@ ./cmd/kognition-memory-policy-gateway

$(TOOL_GATEWAY):
	$(GOBUILD) -o $@ ./cmd/kognition-tool-gateway

$(MODEL_GATEWAY):
	$(GOBUILD) -o $@ ./cmd/kognition-model-gateway

$(AGENT_COMMUNICATION_GATEWAY):
	$(GOBUILD) -o $@ ./cmd/kognition-agent-communication-gateway

$(KNOWLEDGE_GATEWAY):
	$(GOBUILD) -o $@ ./cmd/kognition-knowledge-gateway

$(KNOWLEDGE_AZURESEARCH):
	$(GOBUILD) -o $@ ./cmd/kognition-knowledge-azuresearch

## test: Run all tests
test:
	$(PYTHON) tools/run_bounded.py --timeout $(TEST_TIMEOUT_SECONDS) -- $(GOTEST) -p=1 -count=1 -timeout=3m -race -cover ./...

## integration: Run deterministic public API and controller integration tests
integration:
	$(PYTHON) tools/run_bounded.py --timeout $(TEST_TIMEOUT_SECONDS) -- $(GOTEST) -tags=integration -count=1 -timeout=15m ./tests/integration/...

## lint: Run linters
lint: vet fmt
	@echo "Lint passed"

## quality: Run the full correctness-focused Go quality policy
quality:
	$(GOLANGCI_LINT) run --timeout=10m ./...

## quality-deep: Report complexity and duplication debt
quality-deep:
	$(GOLANGCI_LINT) run --timeout=10m --enable-only=gocognit,gocyclo,dupl ./...

## vet: Run go vet
vet:
	$(GOVET) ./...

## fmt: Check formatting
fmt:
	@test -z "$$($(GOFMT) -l .)" || (echo "Files need formatting:" && $(GOFMT) -l . && exit 1)

## access-catalog: Regenerate the human-role Helm template from the access catalog
access-catalog:
	go run ./tools/access-catalog

## access-catalog-check: Fail if the human-role Helm template is out of date
access-catalog-check:
	go run ./tools/access-catalog --check

## proto: Generate protobuf code
proto:
	buf generate

## proto-lint: Lint proto files
proto-lint:
	buf lint

## docker: Build Docker images (chart-deployed images also tagged with appVersion)
docker:
	docker build -t kognition-apiserver:latest -t kognition-apiserver:$(APP_VERSION) --target apiserver .
	docker build -t kognition-controller-manager:latest -t kognition-controller-manager:$(APP_VERSION) --target controller .
	docker build -t kognitionctl:latest --target ctl .
	docker build -t kognition-k8s-controller:latest -t kognition-k8s-controller:$(APP_VERSION) --target k8s-controller .
	docker build -t kognition-memory-service:latest -t kognition-memory-service:$(APP_VERSION) --target memory-service .
	docker build -t kognition-memory-policy-gateway:latest -t kognition-memory-policy-gateway:$(APP_VERSION) --target memory-policy-gateway .
	docker build -t kognition-tool-gateway:latest -t kognition-tool-gateway:$(APP_VERSION) --target tool-gateway .
	docker build -t kognition-model-gateway:latest -t kognition-model-gateway:$(APP_VERSION) --target model-gateway .
	docker build -t kognition-agent-communication-gateway:latest -t kognition-agent-communication-gateway:$(APP_VERSION) --target agent-communication-gateway .
	docker build -t kognition-intent-gateway:latest -t kognition-intent-gateway:$(APP_VERSION) --target intent-gateway .
	docker build -t kognition-knowledge-gateway:latest -t kognition-knowledge-gateway:$(APP_VERSION) --target knowledge-gateway .
	docker build -t kognition-knowledge-azuresearch:latest -t kognition-knowledge-azuresearch:$(APP_VERSION) --target knowledge-azuresearch .
	docker build -f runtime/agent/Dockerfile -t kognition-agent-runtime:latest -t kognition-agent-runtime:$(APP_VERSION) .
	docker build -f runtime/ingester/Dockerfile -t kognition-kb-ingester:latest -t kognition-kb-ingester:$(APP_VERSION) .

## up: Start local development environment
up:
	docker compose up -d

## down: Stop local development environment
down:
	docker compose down

## services: Manage local services via wrapper (ACTION=<action> SERVICE=<name>)
services:
	@if [ -n "$(SERVICE)" ]; then ./services $(ACTION) $(SERVICE); else ./services $(ACTION); fi

## migrate: Run database migrations
migrate:
	$(GOCMD) run ./cmd/kognition-migrate

## tidy: Tidy go modules
tidy:
	$(GOMOD) tidy

## install-hooks: Install local git hooks for traceability checks
install-hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/pre-commit .githooks/commit-msg
	@echo "Git hooks installed from .githooks"

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## help: Show this help
help:
	@grep -E '^##' Makefile | sed 's/## //' | column -t -s ':'

# ---------------------------------------------------------------------------
# Deployment – Standalone (Helm, default mode)
# ---------------------------------------------------------------------------

## deploy-standalone: Deploy standalone mode via Helm
deploy-standalone: kind-load
	helm upgrade --install $(RELEASE) $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--set apiserver.image.pullPolicy=IfNotPresent \
		--set controllerManager.image.pullPolicy=IfNotPresent \
		--set memoryService.image.pullPolicy=IfNotPresent \
		--set kbPgvector.enabled=true \
		--set kbPgvector.image.pullPolicy=IfNotPresent \
		--set kbIngester.enabled=true \
		--set kbIngester.image.repository=kognition-kb-ingester \
		--set kbIngester.image.pullPolicy=IfNotPresent
	kubectl -n $(NAMESPACE) rollout restart \
		deployment/$(RELEASE)-apiserver \
		deployment/$(RELEASE)-controller-manager \
		deployment/$(RELEASE)-memory-service
	kubectl -n $(NAMESPACE) rollout status deployment/$(RELEASE)-apiserver --timeout=120s
	kubectl -n $(NAMESPACE) rollout status deployment/$(RELEASE)-controller-manager --timeout=120s
	kubectl -n $(NAMESPACE) rollout status deployment/$(RELEASE)-memory-service --timeout=120s

## undeploy-standalone: Remove standalone Helm release
undeploy-standalone:
	helm uninstall $(RELEASE) --namespace $(NAMESPACE) 2>/dev/null || true
	@echo "Release uninstalled. CRDs are retained by Helm design."

# ---------------------------------------------------------------------------
# Deployment – Kubernetes mode (CRD-based, webhooks)
# ---------------------------------------------------------------------------

## deploy-kubernetes: Install a fresh release, or use INSTALL_PLAN for an existing release
deploy-kubernetes:
ifneq ($(strip $(INSTALL_PLAN)),)
	@test -n "$(INSTALL_PLAN)" || { echo "INSTALL_PLAN is required for inactive Kubernetes bootstrap."; exit 1; }
	@test "$(INSTALL_WRITERS_FROZEN)" = "true" || { echo "INSTALL_WRITERS_FROZEN=true is required after controlled drain preparation."; exit 1; }
	$(GOCMD) run ./tools/kognition-install bootstrap --plan "$(INSTALL_PLAN)" --writers-frozen
else
	@$(MAKE) quickstart
endif

## cert-manager: Install cert-manager webhook TLS prerequisites when absent
cert-manager:
	@if kubectl get crd certificates.cert-manager.io >/dev/null 2>&1 && \
		kubectl -n cert-manager rollout status deployment/cert-manager --timeout=5s >/dev/null 2>&1 && \
		kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=5s >/dev/null 2>&1 && \
		kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=5s >/dev/null 2>&1; then \
		exit 0; \
	else \
		echo "==> Installing cert-manager required by Kubernetes webhook TLS..."; \
		helm repo add jetstack https://charts.jetstack.io --force-update; \
		helm upgrade --install cert-manager jetstack/cert-manager --namespace cert-manager --create-namespace --set crds.enabled=true --wait --timeout 5m; \
	fi

## kind-load: Load locally-built images into the kind cluster (best-effort)
kind-load:
	@command -v kind >/dev/null 2>&1 || { echo "==> kind not found; skipping image load."; exit 0; }; \
	clusters=$$(kind get clusters 2>/dev/null); \
	[ -n "$$clusters" ] || { echo "==> No kind clusters found; skipping image load."; exit 0; }; \
	cluster=$$(echo "$$clusters" | head -1); \
	ctx=$$(kubectl config current-context 2>/dev/null); \
	case "$$ctx" in kind-*) name=$${ctx#kind-}; echo "$$clusters" | grep -qx "$$name" && cluster=$$name;; esac; \
	if [ -n "$(FRESH_IMAGE_TAG)" ]; then \
		tags="kognition-k8s-controller:$(FRESH_IMAGE_TAG) kognition-memory-service:$(FRESH_IMAGE_TAG) kognition-memory-policy-gateway:$(FRESH_IMAGE_TAG) kognition-tool-gateway:$(FRESH_IMAGE_TAG) kognition-model-gateway:$(FRESH_IMAGE_TAG) kognition-agent-communication-gateway:$(FRESH_IMAGE_TAG) kognition-intent-gateway:$(FRESH_IMAGE_TAG) kognition-agent-runtime:$(FRESH_IMAGE_TAG) kognition-kb-ingester:$(FRESH_IMAGE_TAG)"; \
	else \
		tags="kognition-apiserver:$(APP_VERSION) kognition-controller-manager:$(APP_VERSION) kognition-k8s-controller:$(APP_VERSION) kognition-memory-service:$(APP_VERSION) kognition-memory-policy-gateway:$(APP_VERSION) kognition-tool-gateway:$(APP_VERSION) kognition-model-gateway:$(APP_VERSION) kognition-agent-communication-gateway:$(APP_VERSION) kognition-intent-gateway:$(APP_VERSION) kognition-agent-runtime:$(APP_VERSION) kognition-kb-ingester:$(APP_VERSION)"; \
		if docker image inspect pgvector/pgvector:pg16 >/dev/null 2>&1; then \
			echo "Preparing pgvector/pgvector:pg16 for kind import"; \
			printf 'FROM pgvector/pgvector:pg16\n' | docker build --provenance=false --platform linux/amd64 --load -t pgvector/pgvector:pg16-kind -f - . >/dev/null && \
				docker tag pgvector/pgvector:pg16-kind pgvector/pgvector:pg16 || \
				echo "  WARN: could not prepare pgvector/pgvector:pg16; kind may fall back to pulling it."; \
			tags="$$tags pgvector/pgvector:pg16"; \
		fi; \
	fi; \
	for tag in $$tags; do \
		docker image inspect "$$tag" >/dev/null 2>&1 || continue; \
		for node in $$(kind get nodes --name "$$cluster" 2>/dev/null); do \
			docker exec "$$node" ctr --namespace=k8s.io images remove "$$tag" >/dev/null 2>&1 || true; \
		done; \
		echo "kind load docker-image $$tag --name $$cluster"; \
		if command -v timeout >/dev/null 2>&1; then \
			timeout 180s kind load docker-image "$$tag" --name "$$cluster"; rc=$$?; \
		else \
			kind load docker-image "$$tag" --name "$$cluster"; rc=$$?; \
		fi; \
		if [ $$rc -ne 0 ]; then echo "  WARN: failed or timed out loading $$tag; node will fall back to pulling it."; fi; \
	done

## undeploy-kubernetes: Remove Kubernetes mode Helm release
undeploy-kubernetes:
	helm uninstall $(RELEASE) --namespace $(NAMESPACE) 2>/dev/null || true
	@echo "Release uninstalled. CRDs are retained by Helm design."

## foundry-creds: Regenerate foundry-creds.yaml from .env and apply it (AGENT_NAMESPACE=ns1,ns2 for multiple)
foundry-creds:
	@if [ ! -f .env ]; then echo "==> No .env file; skipping foundry-creds."; exit 0; fi; \
	strip() { sed 's/^[^=]*=//; s/^[[:space:]]*//; s/[[:space:]]*$$//; s/^"//; s/"$$//'; }; \
	tenant=$$(grep -E '^[[:space:]]*AZURE_TENANT_ID[[:space:]]*=' .env | head -1 | strip); \
	client=$$(grep -E '^[[:space:]]*AZURE_CLIENT_ID[[:space:]]*=' .env | head -1 | strip); \
	secret=$$(grep -E '^[[:space:]]*AZURE_CLIENT_SECRET[[:space:]]*=' .env | head -1 | strip); \
	ctype=$$(grep -E '^[[:space:]]*AZURE_CREDENTIAL_TYPE[[:space:]]*=' .env | head -1 | strip); \
	ctype=$${ctype:-client-secret}; \
	if [ -z "$$tenant" ] || [ -z "$$client" ]; then echo "==> .env missing AZURE_TENANT_ID/CLIENT_ID; skipping."; exit 0; fi; \
	if [ "$$ctype" = "client-secret" ] && [ -z "$$secret" ]; then echo "==> .env missing AZURE_CLIENT_SECRET; skipping."; exit 0; fi; \
	data="{\"clientId\":\"$$client\",\"clientSecret\":\"$$secret\",\"credentialType\":\"$$ctype\",\"type\":\"azure-identity\",\"tenantId\":\"$$tenant\"}"; \
	namespaces=$$(echo "$(AGENT_NAMESPACE)" | tr ',' ' '); \
	: > foundry-creds.yaml; \
	echo "# Azure AD credentials for Foundry, generated from .env by 'make foundry-creds'. Gitignored." >> foundry-creds.yaml; \
	for ns in $$namespaces; do \
	  printf '%s\n' \
	    "---" \
	    "apiVersion: kognition.dev/v1alpha1" \
	    "kind: SecretRef" \
	    "metadata:" \
	    "  name: foundry-creds" \
	    "  namespace: $$ns" \
	    "  annotations:" \
	    "    kognition.dev/internal-secret-data: '$$data'" \
	    "spec:" \
	    "  backendType: internal" \
	    "  backendRef: foundry-creds" \
	    "  key: token" >> foundry-creds.yaml; \
	done; \
	echo "==> Wrote foundry-creds.yaml (SecretRef in: $$namespaces; $$ctype)."; \
	if command -v kubectl >/dev/null 2>&1 && kubectl get crd secretrefs.kognition.dev >/dev/null 2>&1; then \
	  for ns in $$namespaces; do kubectl create namespace $$ns --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - >/dev/null 2>&1 || true; done; \
	  kubectl apply -f foundry-creds.yaml; \
	else echo "==> SecretRef CRD not installed; skipping apply."; fi

# ---------------------------------------------------------------------------
# One-shot install / uninstall
# ---------------------------------------------------------------------------

## install: Install the platform (MODE=standalone|kubernetes, default standalone)
install:
ifeq ($(MODE),kubernetes)
	@$(MAKE) deploy-kubernetes
else ifeq ($(MODE),standalone)
	@$(MAKE) deploy-standalone
else
	@echo "Invalid MODE '$(MODE)'. Use MODE=standalone (default) or MODE=kubernetes."; exit 1
endif

## quickstart: Install a new Kubernetes release and start its controller after checks
quickstart:
	@test -n "$(KUBECONFIG)" -a -n "$(CHART_PACKAGE)" -a -n "$(INSTALL_VALUES)" -a -n "$(WORKLOAD_NAMESPACES)" || { echo "Set KUBECONFIG, CHART_PACKAGE, INSTALL_VALUES and WORKLOAD_NAMESPACES; see docs/getting-started/kubernetes.md."; exit 1; }
	@test "$(INSTALL_EXCLUSIVE)" = "true" || { echo "INSTALL_EXCLUSIVE=true confirms exclusive writes to the installation namespaces."; exit 1; }
	$(GOCMD) run ./tools/kognition-install fresh --kubeconfig "$(KUBECONFIG)" --context "$(KUBE_CONTEXT)" --chart "$(CHART_PACKAGE)" --values "$(INSTALL_VALUES)" --namespace "$(NAMESPACE)" --release "$(RELEASE)" --workload-namespaces "$(WORKLOAD_NAMESPACES)" --authority-namespace "$(if $(INSTALL_AUTHORITY_NAMESPACE),$(INSTALL_AUTHORITY_NAMESPACE),kognition-installation-authority)" --state-dir "$(if $(INSTALL_STATE),$(INSTALL_STATE),.local/kognition-install)" --replicas "$(if $(INSTALL_REPLICAS),$(INSTALL_REPLICAS),1)" --exclusive

## fresh-kubernetes-demo: Legacy alias for quickstart; never resets a cluster
fresh-kubernetes-demo:
	@echo "Legacy alias: installs a new release only; never resets a cluster or installs samples."
	@$(MAKE) quickstart

## uninstall: Uninstall EVERYTHING (Helm release, CRDs, and namespace)
uninstall:
	@echo "==> Gracefully deleting Kognition custom resources (controller still running)..."
	-kubectl get crd -o name 2>/dev/null | grep '\.kognition\.dev$$' | sed 's#.*/##; s#\..*##' | xargs -r -I{} kubectl delete {} --all --all-namespaces --wait=false --ignore-not-found 2>/dev/null
	@echo "==> Uninstalling Helm release '$(RELEASE)' from namespace '$(NAMESPACE)'..."
	-helm uninstall $(RELEASE) --namespace $(NAMESPACE) 2>/dev/null
	@echo "==> Removing finalizers from any remaining Kognition resources (controller is gone)..."
	-for k in $$(kubectl get crd -o name 2>/dev/null | grep '\.kognition\.dev$$' | sed 's#.*/##; s#\..*##'); do \
		for r in $$(kubectl get $$k -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null); do \
			ns=$${r%%/*}; name=$${r#*/}; \
			if [ -n "$$ns" ]; then kubectl patch $$k $$name -n $$ns --type merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true; \
			else kubectl patch $$k $$name --type merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true; fi; \
		done; \
	done
	@echo "==> Deleting Kognition CRDs (cascades to all custom resources)..."
	-kubectl get crd -o name 2>/dev/null | grep '\.kognition\.dev$$' | xargs -r kubectl delete --ignore-not-found --wait=false
	@echo "==> Deleting namespace '$(NAMESPACE)'..."
	-kubectl delete namespace $(NAMESPACE) --ignore-not-found --wait=false 2>/dev/null
	@echo "==> Kognition fully uninstalled."

# ---------------------------------------------------------------------------
# Docker – mode-specific builds
# ---------------------------------------------------------------------------

## docker-k8s: Build only Kubernetes-mode images
docker-k8s:
	docker build -t kognition-k8s-controller:latest -t kognition-k8s-controller:$(APP_VERSION) --target k8s-controller .
	docker build -t kognition-memory-service:latest -t kognition-memory-service:$(APP_VERSION) --target memory-service .
	docker build -t kognition-memory-policy-gateway:latest -t kognition-memory-policy-gateway:$(APP_VERSION) --target memory-policy-gateway .
	docker build -t kognition-tool-gateway:latest -t kognition-tool-gateway:$(APP_VERSION) --target tool-gateway .
	docker build -t kognition-model-gateway:latest -t kognition-model-gateway:$(APP_VERSION) --target model-gateway .
	docker build -t kognition-agent-communication-gateway:latest -t kognition-agent-communication-gateway:$(APP_VERSION) --target agent-communication-gateway .
	docker build -t kognition-intent-gateway:latest -t kognition-intent-gateway:$(APP_VERSION) --target intent-gateway .
	docker build -t kognition-knowledge-gateway:latest -t kognition-knowledge-gateway:$(APP_VERSION) --target knowledge-gateway .
	docker build -t kognition-knowledge-azuresearch:latest -t kognition-knowledge-azuresearch:$(APP_VERSION) --target knowledge-azuresearch .
	docker build -f runtime/agent/Dockerfile -t kognition-agent-runtime:latest -t kognition-agent-runtime:$(APP_VERSION) .
	docker build -f runtime/ingester/Dockerfile -t kognition-kb-ingester:latest -t kognition-kb-ingester:$(APP_VERSION) .

## docker-standalone: Build only standalone-mode images
docker-standalone:
	docker build -t kognition-apiserver:latest -t kognition-apiserver:$(APP_VERSION) --target apiserver .
	docker build -t kognition-controller-manager:latest -t kognition-controller-manager:$(APP_VERSION) --target controller .
	docker build -t kognition-memory-service:latest -t kognition-memory-service:$(APP_VERSION) --target memory-service .
	docker build -f runtime/ingester/Dockerfile -t kognition-kb-ingester:latest -t kognition-kb-ingester:$(APP_VERSION) .

# ---------------------------------------------------------------------------
# Samples
# ---------------------------------------------------------------------------

## samples-list: List available samples
samples-list:
	@echo "Available samples:"
	@for d in samples/*/; do echo "  $$(basename $$d)"; done 2>/dev/null || true
	@for f in samples/*.yaml; do [ -f "$$f" ] && echo "  $$(basename $$f)"; done 2>/dev/null || true

## samples-install: Install samples (SAMPLE=<name> for one, or all)
samples-install:
	pwsh -NoProfile -File make.ps1 samples-install $(if $(SAMPLE),"SAMPLE=$(SAMPLE)") "NAMESPACE=$(NAMESPACE)" "RELEASE=$(RELEASE)" "AGENT_NAMESPACE=$(AGENT_NAMESPACE)"

## samples-full-install: Install and validate every compatible self-contained sample through the strict PowerShell bundle
samples-full-install:
	pwsh -NoProfile -File make.ps1 samples-full-install

## samples-provision-azure: Provision local-test Azure Blob storage and write ignored .env settings
samples-provision-azure:
	pwsh -NoProfile -File make.ps1 samples-provision-azure

## samples-uninstall: Uninstall samples (SAMPLE=<name> for one, or all)
samples-uninstall:
ifdef SAMPLE
	@if [ -f "samples/$(SAMPLE)/Chart.yaml" ]; then \
		echo "helm uninstall $(SAMPLE) --namespace $(AGENT_NAMESPACE)"; \
		helm uninstall $(SAMPLE) --namespace $(AGENT_NAMESPACE) 2>/dev/null || true; \
	elif [ -f "samples/$(SAMPLE)/apply.sh" ]; then \
		echo "kubectl delete -f samples/$(SAMPLE)"; \
		kubectl delete -f "samples/$(SAMPLE)" --ignore-not-found --wait=false 2>/dev/null || true; \
	elif [ -f "samples/$(SAMPLE)/manifests.yaml" ]; then \
		echo "kubectl delete -f samples/$(SAMPLE)/manifests.yaml"; \
		kubectl delete -f samples/$(SAMPLE)/manifests.yaml --ignore-not-found --wait=false 2>/dev/null || true; \
	elif [ -f "samples/$(SAMPLE)" ]; then \
		echo "kubectl delete -f samples/$(SAMPLE)"; \
		kubectl delete -f samples/$(SAMPLE) --ignore-not-found --wait=false 2>/dev/null || true; \
	else \
		echo "Sample '$(SAMPLE)' not found"; exit 1; \
	fi
else
	@echo "Uninstalling all samples..."
	@for d in samples/*/; do \
		[ -d "$$d" ] || continue; \
		if [ -f "$$d/apply.sh" ]; then \
			echo "  kubectl delete -f $$d"; \
			kubectl delete -f "$$d" --ignore-not-found --wait=false 2>/dev/null || true; \
		elif [ -f "$$d/manifests.yaml" ]; then \
			echo "  kubectl delete -f $$d/manifests.yaml"; \
			kubectl delete -f "$$d/manifests.yaml" --ignore-not-found --wait=false 2>/dev/null || true; \
		fi; \
	done; \
	echo "Delete requests submitted; the controller finalizes cleanup in the background."
endif

# ---------------------------------------------------------------------------
# Status & diagnostics
# ---------------------------------------------------------------------------

## status: Show platform and agent status
status:
	@echo "=== Helm Release ==="
	@helm status $(RELEASE) --namespace $(NAMESPACE) --short 2>/dev/null || echo "(not installed)"
	@echo ""
	@echo "=== Pods ($(NAMESPACE)) ==="
	@kubectl get pods -n $(NAMESPACE) -o wide 2>/dev/null || echo "(no pods)"
	@echo ""
	@echo "=== Agents ==="
	@kubectl get agents --all-namespaces 2>/dev/null || echo "(none)"
	@echo ""
	@echo "=== Workflows ==="
	@kubectl get workflows --all-namespaces 2>/dev/null || echo "(none)"
	@echo ""
	@echo "=== Agent Instances ==="
	@kubectl get agentinstances --all-namespaces 2>/dev/null || echo "(none)"
	@echo ""
	@echo "=== Workflow Runs ==="
	@kubectl get workflowruns --all-namespaces 2>/dev/null || echo "(none)"

## logs: Tail platform logs (COMPONENT=<name> to filter)
logs:
ifdef COMPONENT
	kubectl logs -n $(NAMESPACE) -l app.kubernetes.io/name=$(COMPONENT) -f --tail=100
else
	kubectl logs -n $(NAMESPACE) -l app.kubernetes.io/instance=$(RELEASE) -f --tail=50 --max-log-requests=10
endif

## port-forward: Port-forward apiserver to localhost:8080 (standalone mode)
port-forward:
	kubectl port-forward -n $(NAMESPACE) svc/$(RELEASE)-apiserver 8080:8080

## observability-install: Install dev Jaeger, Prometheus Operator, and Grafana
observability-install:
	@if ! helm status $(RELEASE) --namespace $(NAMESPACE) >/dev/null 2>&1; then \
		echo "Helm release '$(RELEASE)' is not installed in namespace '$(NAMESPACE)' yet."; \
		echo "Install Kognition with: make install MODE=kubernetes"; \
		echo "Then rerun: make observability-install"; \
		exit 1; \
	fi
	kubectl create namespace $(OBSERVABILITY_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -n $(OBSERVABILITY_NAMESPACE) -f $(JAEGER_MANIFEST)
	kubectl -n $(OBSERVABILITY_NAMESPACE) rollout status deployment/jaeger --timeout=120s
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
	helm upgrade --install kognition-observability prometheus-community/kube-prometheus-stack --version $(PROMETHEUS_STACK_VERSION) --namespace $(OBSERVABILITY_NAMESPACE) --values $(PROMETHEUS_VALUES) --wait --timeout 10m
	kubectl -n $(OBSERVABILITY_NAMESPACE) rollout status deployment/kognition-observability-grafana --timeout=180s
	kubectl -n $(OBSERVABILITY_NAMESPACE) wait --for=condition=Available deployment -l app=kube-prometheus-stack-operator,release=kognition-observability --timeout=180s
	@echo "Kognition release unchanged. See docs/observability.md to configure telemetry."
	@echo "Jaeger UI: make jaeger-ui, then open http://localhost:16686"
	@echo "Grafana UI: make grafana-ui, then open http://localhost:3000"

## observability: Alias for observability-install
observability: observability-install

## jaeger-install: Backward-compatible alias for observability-install
jaeger-install: observability-install

## jaeger: Alias for jaeger-install
jaeger: jaeger-install

## jaeger-ui: Port-forward the dev Jaeger UI to localhost:16686
jaeger-ui:
	kubectl port-forward -n $(OBSERVABILITY_NAMESPACE) svc/jaeger-collector 16686:16686

## grafana-ui: Port-forward the dev Grafana UI to localhost:3000
grafana-ui:
	kubectl port-forward -n $(OBSERVABILITY_NAMESPACE) svc/kognition-observability-grafana 3000:80

## observability-uninstall: Remove the development observability bundle only
observability-uninstall:
	-kubectl delete -n $(OBSERVABILITY_NAMESPACE) -f $(JAEGER_MANIFEST) --ignore-not-found
	-helm uninstall kognition-observability --namespace $(OBSERVABILITY_NAMESPACE)

## jaeger-uninstall: Backward-compatible alias for observability-uninstall
jaeger-uninstall: observability-uninstall

## studio: Rebuild kognitionctl and launch Studio from bin/ (MODE=standalone|kubernetes)
studio: MODE := kubernetes
studio:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/kognitionctl ./cmd/kognitionctl
	$(BIN_DIR)/kognitionctl studio --mode $(MODE) --namespace $(AGENT_NAMESPACE) --open

## e2e: Run end-to-end tests
e2e:
	pwsh -NoProfile -File e2e/e2e-test.ps1

## e2e-kubernetes-memory: Verify deployed memory gateway and raw-service authorization boundaries
e2e-kubernetes-memory:
	pwsh -NoProfile -File e2e/kubernetes-memory-boundary-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-audit: Verify durable Kubernetes mutation and memory write audit records
e2e-kubernetes-audit:
	pwsh -NoProfile -File e2e/kubernetes-audit-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-tool: Verify deployed Tool Gateway capability exchange, invocation, ledger, and policy revocation
e2e-kubernetes-tool:
	pwsh -NoProfile -File e2e/kubernetes-tool-gateway-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-tool-resilience: Verify deployed Tool Gateway timeout, retry, concurrency, circuit, and metrics behavior
e2e-kubernetes-tool-resilience:
	pwsh -NoProfile -File e2e/kubernetes-tool-gateway-resilience-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-tool-monitoring: Generate Tool Gateway traffic and verify Prometheus scraping, rules, and Grafana dashboard panels
e2e-kubernetes-tool-monitoring: observability-install
	pwsh -NoProfile -File e2e/kubernetes-tool-gateway-monitoring-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ObservabilityNamespace $(OBSERVABILITY_NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-runtime-tool: Verify model-selected Tool execution through the deployed runtime and Tool Gateway
e2e-kubernetes-runtime-tool:
	pwsh -NoProfile -File e2e/kubernetes-runtime-tool-gateway-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-tool-network-policy: Verify Tool Gateway private egress in an enforcing CNI cluster
e2e-kubernetes-tool-network-policy:
	pwsh -NoProfile -File e2e/kubernetes-tool-gateway-network-policy-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-tool-exec-probe: Verify class-approved Tool exec probes in one-shot Kubernetes sandboxes
e2e-kubernetes-tool-exec-probe:
	pwsh -NoProfile -File e2e/kubernetes-tool-exec-probe-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-model: Verify Model Gateway authorization, runtime redaction, and provider credential isolation
e2e-kubernetes-model:
	pwsh -NoProfile -File e2e/kubernetes-model-gateway-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-capability-rotation: Verify active and previous Tool Gateway capability key overlap
e2e-kubernetes-capability-rotation:
	pwsh -NoProfile -File e2e/kubernetes-capability-key-rotation-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)

## e2e-kubernetes-agent-communication: Verify live cross-workspace AgentService delegation through the communication gateway
e2e-kubernetes-agent-communication:
	pwsh -NoProfile -File e2e/kubernetes-agent-communication-gateway-e2e.ps1 -ControllerNamespace $(NAMESPACE) -ReleaseName $(RELEASE)
