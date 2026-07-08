VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MODULE  := github.com/sanchithb/hardware-aware-push-cdn
LDFLAGS := -s -w \
  -X $(MODULE)/pkg/version.Version=$(VERSION) \
  -X $(MODULE)/pkg/version.Commit=$(COMMIT) \
  -X $(MODULE)/pkg/version.Date=$(DATE)

.PHONY: build test test-short vet bench-routing docker release clean

build: ## Build the hpcdn binary into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/hpcdn$(shell go env GOEXE) ./cmd/hpcdn

test: ## Run the full test suite (unit + e2e)
	go test ./...

test-short: ## Run tests without the e2e cluster test
	go test -short ./...

vet:
	go vet ./...

docker: ## Build the multi-role container image
	docker build -f deployments/docker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
	  -t hpcdn:$(VERSION) .

release: ## Cross-compile for the common platforms into ./dist
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/hpcdn-linux-amd64 ./cmd/hpcdn
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/hpcdn-linux-arm64 ./cmd/hpcdn
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/hpcdn-darwin-arm64 ./cmd/hpcdn
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/hpcdn-windows-amd64.exe ./cmd/hpcdn

clean:
	rm -rf bin dist
