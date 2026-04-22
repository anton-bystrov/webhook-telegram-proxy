APP_NAME := webhook-telegram-proxy
GO ?= /usr/local/go/bin/go
GOFMT ?= /usr/local/go/bin/gofmt
GORELEASER ?= ./scripts/run-goreleaser.sh

VERSION ?= $(shell ./scripts/version.sh)
REVISION ?= $(shell git rev-parse --short HEAD)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
DIST_DIR ?= dist
DOCKER_IMAGE ?= ghcr.io/anton-bystrov/$(APP_NAME)
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64,linux/arm/v7

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.revision=$(REVISION) -X main.buildDate=$(BUILD_DATE)

.PHONY: fmt test vet build clean release-check snapshot docker-build docker-buildx changelog

fmt:
	$(GOFMT) -w cmd internal

test:
	GOCACHE=/tmp/go-build-cache GOMODCACHE=/tmp/go-mod-cache $(GO) test ./...

vet:
	GOCACHE=/tmp/go-build-cache GOMODCACHE=/tmp/go-mod-cache $(GO) vet ./...

build:
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOCACHE=/tmp/go-build-cache GOMODCACHE=/tmp/go-mod-cache $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/$(APP_NAME) ./cmd/server

clean:
	rm -rf $(DIST_DIR)

release-check:
	$(GORELEASER) check

snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish --skip=announce

docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(REVISION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(APP_NAME):$(VERSION) .

docker-buildx:
	mkdir -p $(DIST_DIR)
	docker buildx build \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(REVISION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--output type=oci,dest=$(DIST_DIR)/$(APP_NAME)_$(VERSION)_docker.oci \
		-t $(DOCKER_IMAGE):$(VERSION) .

changelog:
	./scripts/changelog.sh $(DIST_DIR)/CHANGELOG.next.md
