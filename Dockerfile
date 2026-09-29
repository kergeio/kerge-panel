# syntax=docker/dockerfile:1

# Build stage. It runs on the build machine's own platform and cross-compiles
# for the target, so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOWORK=off
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN ./scripts/tailwind.sh -i web/input.css -o web/static/app.css --minify

# The license texts of the third-party code in the image: the Go standard
# library and modules compiled into the binary, and the vendored uPlot.
RUN --mount=type=cache,target=/go/pkg/mod \
    mkdir -p /out/doc \
 && ./scripts/third-party-licenses.sh ./cmd/panel \
      "uPlot, vendored in web/static/vendor/uplot=web/static/vendor/uplot/LICENSE" \
      > /out/doc/THIRD_PARTY_LICENSES \
 && cp LICENSE NOTICE /out/doc/

# The agent release the install command pins. Release
# builds set all three; without a version and checksum the panel shows
# placeholders. The repository defaults to the one the panel's code names.
ARG AGENT_REPO="https://github.com/kergeio/kerge-agent"
ARG AGENT_VERSION=""
ARG AGENT_SCRIPT_SHA256=""
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-X github.com/kergeio/kerge-panel/internal/install.Repo=${AGENT_REPO} \
                -X github.com/kergeio/kerge-panel/internal/install.AgentVersion=${AGENT_VERSION} \
                -X github.com/kergeio/kerge-panel/internal/install.ScriptSHA256=${AGENT_SCRIPT_SHA256}" \
      -o /out/kerge-panel ./cmd/panel \
 && mkdir /out/data

# Run stage: no shell, no package manager, uid/gid 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
LABEL org.opencontainers.image.source="https://github.com/kergeio/kerge-panel" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.description="Kerge monitoring panel"
COPY --from=build /out/kerge-panel /kerge-panel
COPY --from=build /out/doc/ /usr/share/doc/kerge-panel/
# An empty /data owned by the panel's user, so that a new named volume
# starts out writable. Bind mounts need the owner set on the host instead.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 3000
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/kerge-panel", "healthcheck"]
ENTRYPOINT ["/kerge-panel"]
