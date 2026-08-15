# BUILD_FROM defaults to debian:trixie-slim for docker-compose / direct builds;
# the Home Assistant add-on build overrides it per-arch via build.yaml
# (ghcr.io/home-assistant/{arch}-base-debian:trixie).
ARG BUILD_FROM=debian:trixie-slim

FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=$(cat VERSION)" -o /out/octopus ./cmd/octopus

FROM $BUILD_FROM
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata git jq curl && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/octopus ./octopus
COPY docker-entrypoint.sh ./docker-entrypoint.sh
RUN chmod +x ./octopus ./docker-entrypoint.sh

EXPOSE 8080
CMD ["./docker-entrypoint.sh"]
