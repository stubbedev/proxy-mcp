# Multi-arch build. buildx sets BUILDPLATFORM (where we compile) and
# TARGETOS/TARGETARCH (what we compile for); cross-compiling in the build
# stage is far faster than emulating the whole toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.BuildVersion=${VERSION}" -o /out/proxy-mcp .

# alpine (not scratch/distroless) so operators can add stdio upstream runtimes
# (e.g. npx/uvx) in a derived image; ca-certificates is needed for https
# upstreams + remote config URLs.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/proxy-mcp /usr/local/bin/proxy-mcp
EXPOSE 9090
ENTRYPOINT ["proxy-mcp"]
CMD ["-config", "/config.json"]
