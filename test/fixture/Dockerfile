FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY . .
RUN --mount=type=secret,id=proxy_ca SSL_CERT_FILE=/run/secrets/proxy_ca CGO_ENABLED=0 go build -trimpath -o /operator ./cmd
FROM scratch
COPY --from=build /operator /operator
USER 65532:65532
ENTRYPOINT ["/operator"]
