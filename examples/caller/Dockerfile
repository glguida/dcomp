FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY component ./component
COPY internal/runtimecontract ./internal/runtimecontract
COPY gen ./gen
COPY examples/caller ./examples/caller
COPY cmd/dcomp-healthcheck ./cmd/dcomp-healthcheck
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/caller ./examples/caller
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dcomp-healthcheck ./cmd/dcomp-healthcheck

FROM scratch
LABEL org.opencontainers.image.title="dcomp caller example"
COPY --from=build /out/caller /caller
COPY --from=build /out/dcomp-healthcheck /dcomp-healthcheck
USER 65532:65532
HEALTHCHECK --interval=5s --timeout=3s --retries=3 CMD ["/dcomp-healthcheck", "--socket", "/run/dcomp/in/upstream"]
ENTRYPOINT ["/caller"]
