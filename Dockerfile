# syntax=docker/dockerfile:1

# Stage 1 build from full image.
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
# Cache the module downloads so repeated builds don't re-fetch every dependency.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# Cache the module and compiler caches.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
    -o /out/controller ./cmd/controller

# Stage 2 run lightweight thin static go binary no shell/package manager.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
