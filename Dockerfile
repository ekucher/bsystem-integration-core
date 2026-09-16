FROM golang:1.26-alpine AS build
WORKDIR /src
# go.sum is copied so the build verifies dependency checksums instead of
# regenerating them, which would accept a substituted module silently.
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bsystem-integration-core ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -G app app
COPY --from=build /out/bsystem-integration-core /usr/local/bin/bsystem-integration-core
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/usr/local/bin/bsystem-integration-core"]
