# syntax=docker/dockerfile:1

# Built for each platform from the build machine's own Go, without emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/band3-rooms ./cmd/band3-rooms

# A static binary on distroless: no shell, no package manager, runs as nonroot (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/band3-rooms /band3-rooms

# The health endpoint stays inside the container, for HEALTHCHECK only.
ENV ROOMS_LISTEN=0.0.0.0:19532 \
    ROOMS_HEALTH=127.0.0.1:19580
USER nonroot:nonroot
EXPOSE 19532/tcp

HEALTHCHECK --interval=60s --timeout=5s --start-period=10s \
  CMD ["/band3-rooms", "-healthcheck"]

ENTRYPOINT ["/band3-rooms"]
