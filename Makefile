# CrowdVM build. `make` builds host binaries into bin/; `make dist` cross-compiles.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# main.version stamps the CLI; the other binaries read their package Version.
LDFLAGS := -s -w -X main.version=$(VERSION) -X github.com/crowdvm/crowdvm/internal/server.Version=$(VERSION) -X github.com/crowdvm/crowdvm/internal/node.Version=$(VERSION) -X github.com/crowdvm/crowdvm/internal/guest.Version=$(VERSION)
GO ?= go

.PHONY: all build guest test e2e dist clean docs release-nightly release-stable hooks

all: build

guest:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o bin/crowdvm-guest ./cmd/crowdvm-guest

build: guest
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/crowdvm ./cmd/crowdvm-server ./cmd/crowdvm-node

# docs/guide/cli-reference.md from the CLI's command tree (served at /docs/cli-reference.md).
docs:
	$(GO) generate ./docs

test:
	$(GO) vet ./...
	$(GO) test ./...

e2e: build
	CROWDVM_E2E=1 $(GO) test -count=1 -timeout 60m ./test/e2e/...

dist: guest
	scripts/dist.sh $(VERSION)

clean:
	rm -rf bin dist

# Local releases (no CI service): see docs/RELEASING.md.
hooks:
	git config core.hooksPath scripts/git-hooks

release-nightly: hooks
	scripts/release/release.sh nightly

release-stable: hooks
	scripts/release/release.sh stable
