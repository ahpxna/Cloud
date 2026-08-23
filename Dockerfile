# syntax=docker/dockerfile:1.7
FROM golang:1.26.7-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/upload-gateway ./cmd/upload-gateway \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/admin ./cmd/admin \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/manifest ./cmd/manifest

FROM alpine:3.23 AS runtime-base
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 photo \
    && adduser -S -D -H -u 10001 -G photo photo
USER 10001:10001
EXPOSE 8080

FROM runtime-base AS gateway
COPY --from=build /out/upload-gateway /usr/local/bin/upload-gateway
ENTRYPOINT ["/usr/local/bin/upload-gateway"]

FROM runtime-base AS admin
COPY --from=build /out/admin /usr/local/bin/admin
ENTRYPOINT ["/usr/local/bin/admin"]

FROM runtime-base AS manifest
COPY --from=build /out/manifest /usr/local/bin/manifest
ENTRYPOINT ["/usr/local/bin/manifest"]
