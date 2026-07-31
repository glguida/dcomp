FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY component ./component
COPY gen ./gen
COPY examples/caller ./examples/caller
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/caller ./examples/caller

FROM scratch
LABEL org.opencontainers.image.title="dcomp caller example"
COPY --from=build /out/caller /caller
USER 65532:65532
ENTRYPOINT ["/caller"]
