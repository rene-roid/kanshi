# Build stage. The module has no third-party dependencies, so there is nothing
# to download and nothing to vendor — go.mod is copied on its own only so the
# layer cache survives edits to the source.
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY internal ./internal
COPY web ./web

# CGO off because nothing here needs libc: /proc is read as plain files and the
# Docker socket is a plain unix socket. That is what makes a scratch image
# possible at all.
ENV CGO_ENABLED=0
RUN go vet ./... && \
    go build -trimpath -ldflags="-s -w" -o /kanshi .

# Runtime stage. The web assets are embedded in the binary, so the image is one
# static file and nothing else — no shell, no package manager, no CVE surface.
FROM scratch

COPY --from=build /kanshi /kanshi

EXPOSE 8100

# The probe is the binary itself: a scratch image has no curl to call, and this
# way it reads KANSHI_HOST/KANSHI_PORT from the same code that binds them.
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/kanshi", "-healthcheck"]

ENTRYPOINT ["/kanshi"]
