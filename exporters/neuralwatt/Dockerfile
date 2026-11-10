# ビルドコンテキストはリポジトリのルート（docker build -f exporters/neuralwatt/Dockerfile .）。
FROM golang:1.25.13-bookworm AS build
WORKDIR /src
COPY exporters/neuralwatt/go.mod exporters/neuralwatt/go.sum ./
RUN go mod download
COPY exporters/neuralwatt/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/neuralwatt-exporter .

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/Lepinoid/infra" \
      org.opencontainers.image.description="Prometheus exporter for the Neuralwatt /v1/quota balance and usage"
# api.neuralwatt.com への TLS 接続に必要な CA 証明書（scratch には含まれない）。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/neuralwatt-exporter /neuralwatt-exporter
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/neuralwatt-exporter"]
