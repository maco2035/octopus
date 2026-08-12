# Generic self-host image: `docker build .` / `docker compose up` runs
# Octopus anywhere. The Home Assistant add-on has its own Dockerfile
# (ha-addon/Dockerfile) since it needs the HA base image + bashio, not this
# one — the Go source and build are otherwise identical.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=$(cat VERSION)" -o /out/octopus ./cmd/octopus

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git
WORKDIR /app
COPY --from=build /out/octopus ./octopus
COPY config.example.yaml ./config.example.yaml
RUN mkdir -p /app/data
EXPOSE 8080
ENTRYPOINT ["./octopus"]
