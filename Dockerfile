# Multi-stage build. Stage 1 compiles the Go binaries; stage 2 is a tiny runtime
# image that carries only the binaries — no Go toolchain, no source. This keeps
# the final image small and its attack surface minimal.
#
# We build BOTH binaries into one image and choose which to run via the command
# (see docker-compose.yml / the k8s manifests). One image, two roles.

# ---- Stage 1: build ----
FROM golang:1.25-alpine AS build
WORKDIR /src

# Copy go.mod/go.sum first and download deps. Because these files change rarely,
# Docker caches this layer and skips re-downloading on every code change — a
# standard Go+Docker speed trick.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the source and build. CGO_ENABLED=0 produces a fully static binary
# that runs on a bare/scratch-style image with no libc. -ldflags "-s -w" strips
# debug info to shrink the binaries.
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/worker ./cmd/worker

# ---- Stage 2: runtime ----
# alpine (not scratch) so we get ca-certificates and a shell for debugging. It's
# still only ~10MB. Use scratch if you want the absolute minimum and don't need
# TLS to external services.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
USER app
WORKDIR /app

COPY --from=build /out/server /app/server
COPY --from=build /out/worker /app/worker

# The migrations and dashboard HTML are embedded in the binaries (//go:embed),
# so there is nothing else to copy — the image is just the two executables.

EXPOSE 8080
# Default to the server; docker-compose / k8s override this for worker pods.
ENTRYPOINT ["/app/server"]
