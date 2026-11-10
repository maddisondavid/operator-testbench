FROM golang:1.26.5-bookworm AS development
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=proxy_ca SSL_CERT_FILE=/run/secrets/proxy_ca go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN mkdir -p /out && chmod 1777 /out

FROM development AS build
RUN CGO_ENABLED=0 go build -trimpath -o /out/dynamo ./cmd/dynamo

FROM scratch AS runtime
COPY --from=build /out/dynamo /dynamo
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/dynamo"]
CMD ["--workdir", "/state", "--listen", "0.0.0.0:8080"]
