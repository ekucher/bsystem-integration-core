FROM golang:1.26.8-alpine AS build
WORKDIR /src
# go.sum is copied so the build verifies dependency checksums instead of
# regenerating them, which would accept a substituted module silently.
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bsystem-integration-core ./cmd/server

FROM alpine:3.22
# apk upgrade, not only apk add.
#
# The packages that carry vulnerabilities here are the ones the base image
# already has — openssl, musl, busybox — and `apk add` does not touch them. A
# base tag is rebuilt on its own schedule, so an image built today from
# alpine:3.22 can carry a library that was patched weeks ago and is still
# waiting for the tag to move.
#
# This was found by scanning the product image rather than a mock: the Security
# workflow scans one mock image, on the reasoning that the four mocks share a
# Dockerfile, and nothing had ever scanned the image the platform actually
# ships. The first run that did found CVE-2026-14456 in libssl3 and
# libcrypto3, fixed upstream and not yet in the tag.
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app
COPY --from=build /out/bsystem-integration-core /usr/local/bin/bsystem-integration-core
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/bsystem-integration-core"]
