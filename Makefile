VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/lassejlv/ai-cli/internal/cli.Version=$(VERSION)

.PHONY: build install test vet fmt

build:
	go build -ldflags "$(LDFLAGS)" -o ai ./cmd/ai

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/ai

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .
