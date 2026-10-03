# syntax=docker/dockerfile:1

# The module targets Go 1.24 (go.mod), but release binaries are built with a
# currently supported toolchain so they pick up standard-library security fixes.
ARG GO_VERSION=1.26

# ---- build stage: full Go toolchain, discarded after the build ----
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
# Overridable for mirrors / air-gapped builds; the default is the public module proxy.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
# Download modules in their own layer so code changes don't refetch them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/upstream ./cmd/upstream

# ---- runtime stage: no shell, no package manager, runs as uid 65532 ----
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="floodgate" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/gateway /out/upstream /usr/local/bin/
COPY examples/config.yaml /etc/rlgw/config.yaml
ENV RLGW_CONFIG=/etc/rlgw/config.yaml
USER nonroot:nonroot
EXPOSE 8080 9090
HEALTHCHECK --interval=10s --timeout=3s CMD ["/usr/local/bin/gateway", "-probe", "http://127.0.0.1:9090/healthz"]
ENTRYPOINT ["/usr/local/bin/gateway"]
