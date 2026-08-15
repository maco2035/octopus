# One image, two consumers: `docker build .` / docker-compose runs this
# directly (default BUILD_FROM below); .github/workflows/publish-ha-addon.yml
# builds the exact same Dockerfile per-arch with BUILD_FROM overridden to
# ghcr.io/home-assistant/{arch}-base and pushes the result to GHCR, which
# is what the HA add-on actually pulls (config.yaml's `image:` field) —
# Supervisor never builds this Dockerfile itself. Both bases are
# Alpine-derived, so the apk install and everything after it is identical
# either way — docker-entrypoint.sh is what actually tells the two modes
# apart at runtime. ARG must be declared before the first FROM to be
# usable in the second FROM (Docker's global-scope ARG rule).
ARG BUILD_FROM=alpine:3.20

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=$(cat VERSION)" -o /out/octopus ./cmd/octopus

FROM $BUILD_FROM
# git: needed at *runtime*, not just to build this image — internal/tools
# shells out to it for the real clone/commit/push work every pipeline run
# does. bash: bashio's own lib fails under plain POSIX sh (verified
# against the real HA base image), and docker-entrypoint.sh needs it
# either way; harmless on the plain-Docker path, which never calls a
# bashio:: function.
RUN apk add --no-cache ca-certificates git bash

WORKDIR /app
COPY --from=build /out/octopus ./octopus
COPY docker-entrypoint.sh ./docker-entrypoint.sh
RUN chmod +x ./octopus ./docker-entrypoint.sh

EXPOSE 8080
CMD ["./docker-entrypoint.sh"]
