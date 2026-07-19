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
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
    -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
