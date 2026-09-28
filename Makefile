# goipslad / goipsla
#
# make build          static binaries in bin/ (seen as /work/bin in the staging source container)
# make test           unit tests with the race detector
# make staging-up     start the staging environment (staging/compose.yaml)
# make staging-run    build and run goipslad in the staging source container (CONFIG=path to override)
#
# Run "make help" for every target.

GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
BIN_DIR  := bin
LDFLAGS  := -s -w -X main.version=$(VERSION)
COMPOSE  := docker compose -f staging/compose.yaml
ITEST_DIR := $(BIN_DIR)/itest

.DEFAULT_GOAL := build

.PHONY: help all build test vet lint fmt tidy clean integration-test staging-integration-test \
	staging-up staging-rebuild staging-down staging-verify staging-run
.PHONY: dist image

help: ## show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-26s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

all: vet test build ## vet, test and build

build: ## build static bin/goipslad, bin/goipsla and the tools (bin/probe-echo, bin/metrics-get)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ ./cmd/goipslad ./cmd/goipsla ./tools/probe-echo ./tools/metrics-get

test: ## run unit tests with the race detector (needs cgo)
	$(GO) test -race ./...

vet: ## run go vet
	$(GO) vet ./...

lint: ## run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed. Install v2 with:" >&2; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest" >&2; \
		echo "or see https://golangci-lint.run/welcome/install/" >&2; \
		exit 1; }
	golangci-lint run

fmt: ## format the sources with gofmt
	gofmt -s -w cmd internal

tidy: ## tidy go.mod and go.sum
	$(GO) mod tidy

clean: ## remove build outputs
	rm -rf $(BIN_DIR) dist

integration-test: ## run integration tests (meant for the staging source container; tests skip elsewhere)
	$(GO) test -tags=integration ./...

# The staging containers have no Go toolchain, so the test binaries are built
# on the host and executed in the source container through the /work mount,
# each in its package directory. ITEST_FLAGS is passed to every test binary.
# Every package runs even when an earlier one fails; the failed packages are
# listed at the end and make the target fail.
staging-integration-test: ## build integration test binaries on the host and run them in the source container
	@rm -rf $(ITEST_DIR); mkdir -p $(ITEST_DIR); failed=""; \
	for dir in $$($(GO) list -tags=integration -f '{{if or .TestGoFiles .XTestGoFiles}}{{.Dir}}{{end}}' ./...); do \
		rel=$${dir#$(CURDIR)/}; \
		bin=$(ITEST_DIR)/$$(echo "$$rel" | tr / .).test; \
		echo "== $$rel"; \
		if ! CGO_ENABLED=0 $(GO) test -c -tags=integration -o "$$bin" "./$$rel"; then failed="$$failed $$rel(build)"; continue; fi; \
		$(COMPOSE) exec -T -w "/work/$$rel" source "/work/$$bin" $(ITEST_FLAGS) || failed="$$failed $$rel"; \
	done; \
	if [ -n "$$failed" ]; then echo "FAILED:$$failed" >&2; exit 1; fi; echo "all integration test packages passed"

# Every build of the node image gets a new image ID even when all layers are
# cached, and "up --build" then recreates all containers (dropping netem
# qdiscs and anything running in them). staging-up therefore builds the image
# only when it is missing; use staging-rebuild after changing staging/node/.
staging-up: ## start the staging environment (reuses running containers)
	$(COMPOSE) up -d

staging-rebuild: ## rebuild the node image and recreate the staging containers
	$(COMPOSE) up -d --build

staging-down: ## stop and remove the staging environment
	$(COMPOSE) down

staging-verify: ## check reachability in the staging environment
	staging/scripts/verify.sh

staging-run: ## build and run goipslad in the source container (CONFIG=path)
	staging/scripts/run-goipslad.sh $(CONFIG)

# ---- distribution (P8) ----
#
# make dist VERSION=1.2.3   dist/goipsla-1.2.3-linux-{amd64,arm64}.tar.gz
# make image                the container image goipsla:$(IMAGE_TAG)

DIST_DIR    := dist
DIST_ARCHES ?= amd64 arm64
IMAGE       ?= goipsla
IMAGE_TAG   ?= dev

dist: ## build release tarballs dist/goipsla-$(VERSION)-linux-{amd64,arm64}.tar.gz
	@set -e; mkdir -p $(DIST_DIR); \
	for arch in $(DIST_ARCHES); do \
		name=goipsla-$(VERSION)-linux-$$arch; stage=$(DIST_DIR)/$$name; \
		rm -rf $$stage; mkdir -p $$stage/docs; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $$stage/ ./cmd/goipslad ./cmd/goipsla; \
		cp packaging/systemd/goipslad.service packaging/config.example.yaml README.md $$stage/; \
		cp docs/*.md $$stage/docs/; \
		tar -C $(DIST_DIR) -czf $(DIST_DIR)/$$name.tar.gz --owner=0 --group=0 $$name; \
		rm -rf $$stage; \
		echo "$(DIST_DIR)/$$name.tar.gz"; \
	done

image: ## build the container image $(IMAGE):$(IMAGE_TAG) (packaging/docker/Dockerfile)
	DOCKER_BUILDKIT=1 docker build -f packaging/docker/Dockerfile \
		--build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) .
