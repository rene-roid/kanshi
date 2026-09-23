# Build stage. It always runs on the build machine's own architecture and
# cross-compiles for the target, which Go does natively — so a multi-arch
# image builds at full speed with no QEMU emulation. The module has no
# third-party dependencies, so there is nothing to download.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod ./
COPY main.go main_other.go ./
COPY internal ./internal
COPY web ./web

# CGO off because nothing here needs libc: /proc is read as plain files and the
# Docker socket is a plain unix socket. That is what makes a scratch image
# possible at all.
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" -o /kanshi .

# Runtime stage. The web assets are embedded in the binary, so the image is one
# static file and nothing else — no shell, no package manager, no CVE surface.
FROM scratch

ARG VERSION=dev
LABEL org.opencontainers.image.title="kanshi" \
      org.opencontainers.image.description="A one-page, mobile-first glance at a homeserver: CPU, memory, a storage treemap and Docker containers." \
      org.opencontainers.image.source="https://github.com/rene-roid/kanshi" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /kanshi /kanshi

# The Compose file mounts the host's / here; the storage map then finds the
# host's drives under /hostfs/mnt on its own.
ENV KANSHI_HOST_ROOT=/hostfs

EXPOSE 8100

# The probe is the binary itself: a scratch image has no curl to call. It asks
# 127.0.0.1, which is listened on in every access mode.
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/kanshi", "-healthcheck"]

ENTRYPOINT ["/kanshi"]
