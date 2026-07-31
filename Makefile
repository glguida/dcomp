GO ?= go
PROTOC ?= protoc
GO_BIN := $(or $(shell $(GO) env GOBIN),$(shell $(GO) env GOPATH)/bin)

PROTOC_VERSION := 35.1
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

.PHONY: all build test test-race tools generate examples integration clean

all: build

build:
	mkdir -p bin
	$(GO) build -buildvcs=false -trimpath -o bin/dcomp ./cmd/dcomp
	$(GO) build -buildvcs=false -trimpath -o bin/dcomp-healthcheck ./cmd/dcomp-healthcheck

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

generate:
	@test "$$($(PROTOC) --version)" = "libprotoc $(PROTOC_VERSION)" || \
		{ echo "dcomp generation requires protoc $(PROTOC_VERSION)" >&2; exit 1; }
	@test "$$($(GO_BIN)/protoc-gen-go --version)" = "protoc-gen-go $(PROTOC_GEN_GO_VERSION)" || \
		{ echo "install protoc-gen-go $(PROTOC_GEN_GO_VERSION) with 'make tools'" >&2; exit 1; }
	@test "$$($(GO_BIN)/protoc-gen-go-grpc --version)" = "protoc-gen-go-grpc $(patsubst v%,%,$(PROTOC_GEN_GO_GRPC_VERSION))" || \
		{ echo "install protoc-gen-go-grpc $(PROTOC_GEN_GO_GRPC_VERSION) with 'make tools'" >&2; exit 1; }
	PATH="$(GO_BIN):$$PATH" $(PROTOC) \
		--go_out=. --go_opt=module=github.com/glguida/dcomp \
		--go-grpc_out=. --go-grpc_opt=module=github.com/glguida/dcomp \
		protocol/dcomp/example/v1/echo.proto
	$(GO) fmt ./gen/...

examples:
	docker build -f examples/echo/Dockerfile -t dcomp-example-echo:dev .
	docker build -f examples/uppercase/Dockerfile -t dcomp-example-uppercase:dev .
	docker build -f examples/caller/Dockerfile -t dcomp-example-caller:dev .

integration: build examples
	tools/integration-test

clean:
	rm -rf bin
