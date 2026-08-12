# Home Assistant add-on image. BUILD_FROM comes from build.yaml per-arch and
# provides bashio + s6-overlay; the generic self-host image at the repo
# root (../Dockerfile) is unrelated to this one.
ARG BUILD_FROM
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=$(cat VERSION)" -o /out/octopus ./cmd/octopus

FROM ${BUILD_FROM}
RUN apk add --no-cache git

COPY --from=build /out/octopus /usr/bin/octopus
COPY ha-addon/run.sh /run.sh
RUN chmod a+x /run.sh /usr/bin/octopus

CMD ["/run.sh"]
