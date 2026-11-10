#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
docker run --rm \
  -v "$PWD:/src" -w /src \
  -v dynamo-go-modules:/go/pkg/mod -v dynamo-go-cache:/tmp/gocache \
  -v /etc/ssl/certs/ca-certificates.crt:/run/proxy-ca.pem:ro \
  -e SSL_CERT_FILE=/run/proxy-ca.pem \
  golang:1.26.5-bookworm sh -c '
    go mod download
    chmod -R a+rX /go/pkg/mod
    chmod 777 /tmp/gocache
    runuser -u nobody -- env GOCACHE=/tmp/gocache go vet ./cmd/... ./internal/...
    runuser -u nobody -- env GOCACHE=/tmp/gocache go test -race -count=1 ./cmd/... ./internal/...
  '
