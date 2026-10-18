# Build the manager binary
# The builder is pinned by tag and manifest-list digest, so it changes only
# with a commit here: the 1.26 line's current patch. The image sets
# GOTOOLCHAIN=local, so it builds with its own Go and never downloads the one
# go.mod's toolchain line names; go.mod's go line must stay within 1.26.
FROM golang:1.26.9@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
# Pinned 2026-10-18 to the nonroot tag's index (manifest-list) digest, like the
# builder, so the runtime base changes only with a commit here (#60, I6). The
# index carries linux/amd64, which GhcrPublish builds, and linux/arm64.
# Refresh: the Docker-Content-Digest this prints is the new pin.
# curl -fsSI -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' https://gcr.io/v2/distroless/static/manifests/nonroot | grep -i docker-content-digest
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
