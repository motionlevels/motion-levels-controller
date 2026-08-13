ARG GO_IMAGE=golang:1.24-bookworm

FROM ${GO_IMAGE} AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG BUILD_REVISION=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY contracts ./contracts
COPY internal ./internal
RUN go test ./... \
    && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath -ldflags="-s -w -X main.buildRevision=${BUILD_REVISION}" \
      -o /out/motion-levels-controller ./cmd/motion-levels-controller

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      zstd \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 motionlevels \
    && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent motionlevels
ARG BUILD_REVISION=unknown
ARG BUILD_CREATED_AT=
ARG CONTROLLER_PROTOCOL_VERSION=v1
LABEL org.opencontainers.image.title="Motion Levels Controller"
LABEL org.opencontainers.image.source="https://github.com/motionlevels/motion-levels-controller"
LABEL org.opencontainers.image.revision="${BUILD_REVISION}"
LABEL org.opencontainers.image.created="${BUILD_CREATED_AT}"
LABEL io.motionlevels.controller.protocol="${CONTROLLER_PROTOCOL_VERSION}"
COPY --from=build /out/motion-levels-controller /app/bin/motion-levels-controller
COPY deploy/entrypoint.sh /usr/local/bin/motion-levels-controller
RUN /bin/sh -n /usr/local/bin/motion-levels-controller
USER 10001:10001
HEALTHCHECK --interval=5s --timeout=3s --retries=12 --start-period=10s \
  CMD curl -fsS http://127.0.0.1:4101/health || exit 1
ENTRYPOINT ["/usr/local/bin/motion-levels-controller"]
