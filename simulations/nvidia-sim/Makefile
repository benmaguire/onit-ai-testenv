IMG ?= ghcr.io/benmaguire/onit-ai-testenv/simulator
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.2

.PHONY: all build test generate manifests catalog docker-build docker-push run-agent verify

all: generate manifests catalog build test

build: ## Build simulator and simctl into bin/
	go build -ldflags "-X main.version=$(VERSION)" -o bin/simulator ./cmd/simulator
	go build -o bin/simctl ./cmd/simctl

test:
	go vet ./...
	go test ./...

generate: ## DeepCopy methods
	$(CONTROLLER_GEN) object paths=./api/...

manifests: ## CRDs and ClusterRole from kubebuilder markers
	$(CONTROLLER_GEN) crd rbac:roleName=testenv-simulator paths=./... \
	  output:crd:artifacts:config=deploy/base/crds output:rbac:artifacts:config=deploy/base/rbac

catalog: ## Regenerate CATALOG.md from the registry
	go run ./hack/gencatalog > CATALOG.md

verify: all ## Fail if generated files are stale
	git diff --exit-code -- api deploy CATALOG.md

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMG):$(VERSION) .

docker-push: docker-build
	docker push $(IMG):$(VERSION)

run-agent: build ## Run against the current kubeconfig, no auth (development)
	bin/simulator --mode=agent --api-addr=:8443 --metrics-addr=0 --health-probe-addr=:8081 --namespace=sim-system
