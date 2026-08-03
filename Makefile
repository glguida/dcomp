GO ?= go
PROTOC ?= protoc
INSTALL ?= install
CGO_ENABLED ?= 0

PREFIX ?= /usr/local
DESTDIR ?=
BINDIR ?= $(PREFIX)/bin
DOCDIR ?= $(PREFIX)/share/doc/dcomp

GO_BIN := $(or $(shell $(GO) env GOBIN),$(shell $(GO) env GOPATH)/bin)

PROTOC_VERSION := 35.1
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

.PHONY: all build test test-race install-test tools generate examples integration install uninstall clean

all: build

build:
	mkdir -p bin
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -buildvcs=false -trimpath -o bin/dcomp ./cmd/dcomp
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -buildvcs=false -trimpath -o bin/dcomp-healthcheck ./cmd/dcomp-healthcheck

test:
	$(GO) test ./...
	tools/install-test

test-race:
	$(GO) test -race ./...

install-test:
	tools/install-test

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

install:
	@test -x bin/dcomp && test -x bin/dcomp-healthcheck || { \
		echo "dcomp: run 'make build' before 'make install'" >&2; \
		exit 1; \
	}
	[ -d "$(DESTDIR)$(BINDIR)" ] || $(INSTALL) -d -m 0755 "$(DESTDIR)$(BINDIR)"
	$(INSTALL) -m 0755 bin/dcomp "$(DESTDIR)$(BINDIR)/dcomp"
	$(INSTALL) -m 0755 bin/dcomp-healthcheck "$(DESTDIR)$(BINDIR)/dcomp-healthcheck"
	[ -d "$(DESTDIR)$(DOCDIR)/docs" ] || $(INSTALL) -d -m 0755 "$(DESTDIR)$(DOCDIR)/docs"
	$(INSTALL) -m 0644 README.md "$(DESTDIR)$(DOCDIR)/README.md"
	$(INSTALL) -m 0644 LICENSE "$(DESTDIR)$(DOCDIR)/LICENSE"
	$(INSTALL) -m 0644 docs/architecture.md "$(DESTDIR)$(DOCDIR)/docs/architecture.md"
	$(INSTALL) -m 0644 docs/component-contract.md "$(DESTDIR)$(DOCDIR)/docs/component-contract.md"
	$(INSTALL) -m 0644 docs/lifecycle.md "$(DESTDIR)$(DOCDIR)/docs/lifecycle.md"
	$(INSTALL) -m 0644 docs/prior-art.md "$(DESTDIR)$(DOCDIR)/docs/prior-art.md"

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/dcomp"
	rm -f "$(DESTDIR)$(BINDIR)/dcomp-healthcheck"
	rm -f "$(DESTDIR)$(DOCDIR)/README.md"
	rm -f "$(DESTDIR)$(DOCDIR)/LICENSE"
	rm -f "$(DESTDIR)$(DOCDIR)/docs/architecture.md"
	rm -f "$(DESTDIR)$(DOCDIR)/docs/component-contract.md"
	rm -f "$(DESTDIR)$(DOCDIR)/docs/lifecycle.md"
	rm -f "$(DESTDIR)$(DOCDIR)/docs/prior-art.md"
	rmdir "$(DESTDIR)$(DOCDIR)/docs" 2>/dev/null || true
	rmdir "$(DESTDIR)$(DOCDIR)" 2>/dev/null || true

clean:
	rm -rf bin
