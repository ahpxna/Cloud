# syntax=docker/dockerfile:1.7
ARG GO_BUILD_IMAGE=golang:1.26.7-alpine3.23
ARG RUNTIME_IMAGE=alpine:3.23
FROM ${GO_BUILD_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/upload-gateway ./cmd/upload-gateway \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/admin ./cmd/admin \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/manifest ./cmd/manifest \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scrub ./cmd/scrub \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/metrics-exporter ./cmd/metrics-exporter \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/synthetic-probe ./cmd/synthetic-probe \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/audit-export ./cmd/audit-export \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM ${RUNTIME_IMAGE} AS runtime-base
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

FROM runtime-base AS scrub
COPY --from=build /out/scrub /usr/local/bin/scrub
ENTRYPOINT ["/usr/local/bin/scrub"]

FROM runtime-base AS metrics-exporter
COPY --from=build /out/metrics-exporter /usr/local/bin/metrics-exporter
ENTRYPOINT ["/usr/local/bin/metrics-exporter"]

FROM runtime-base AS synthetic-probe
COPY --from=build /out/synthetic-probe /usr/local/bin/synthetic-probe
ENTRYPOINT ["/usr/local/bin/synthetic-probe"]

FROM runtime-base AS audit-export
COPY --from=build /out/audit-export /usr/local/bin/audit-export
ENTRYPOINT ["/usr/local/bin/audit-export"]

FROM runtime-base AS migrate
COPY --from=build /out/migrate /usr/local/bin/migrate
COPY db/migrations /app/migrations
ENTRYPOINT ["/usr/local/bin/migrate"]

# Keep the Internet-facing gateway as Docker's default final target. CI still
# builds and scans every privileged/offline tool target explicitly.
FROM gateway AS release
