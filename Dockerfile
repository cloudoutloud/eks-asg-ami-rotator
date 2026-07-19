# syntax=docker/dockerfile:1

# Pin the build stage to the native build platform (e.g. arm64 on Apple Silicon)
# so the Go toolchain runs natively and cross-compiles to the target arch. This
# avoids running go under QEMU emulation, which crashes go's DNS resolver during
# "go mod download".
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# Cache the module downloads so repeated builds don't re-fetch every dependency.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# Cache the module and compiler caches. The first build still has to compile the
# large client-go/kubectl/controller-runtime tree (can take several minutes on a
# small Docker VM); subsequent builds reuse this cache and finish in seconds.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
    -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
