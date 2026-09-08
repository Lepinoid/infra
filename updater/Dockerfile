FROM golang:1.25.13-bookworm AS build
WORKDIR /src
COPY updater/go.mod ./
COPY updater/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/updater .

FROM debian:bookworm-slim AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl tar && rm -rf /var/lib/apt/lists/*
WORKDIR /downloads
RUN curl -fsSL https://github.com/oras-project/oras/releases/download/v1.3.4/oras_1.3.4_linux_amd64.tar.gz -o oras.tar.gz \
    && echo 'f27adb935022d94df8dc77719c322dda592c78a0d57a6f7dcdd8d900b248c454  oras.tar.gz' | sha256sum -c - \
    && tar -xzf oras.tar.gz oras
RUN curl -fsSL https://dl.k8s.io/release/v1.34.1/bin/linux/amd64/kubectl -o kubectl \
    && echo '7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3  kubectl' | sha256sum -c - \
    && chmod 755 kubectl
RUN curl -fsSL https://github.com/itzg/mc-monitor/releases/download/0.17.1/mc-monitor_0.17.1_linux_amd64.tar.gz -o monitor.tar.gz \
    && echo '525d6ff1f8e4190957ac76d8d0bcbf3db13c84c2b794a110268f09a3b24661f2  monitor.tar.gz' | sha256sum -c - \
    && tar -xzf monitor.tar.gz mc-monitor

FROM busybox:1.37.0-musl AS toolbox
RUN mkdir -p /out && cp /bin/busybox /out/tar
FROM toolbox AS verify
COPY --from=tools /downloads/oras /downloads/kubectl /downloads/mc-monitor /usr/local/bin/
COPY --from=build /out/updater /usr/local/bin/updater
COPY --from=toolbox /out/tar /usr/local/bin/tar
RUN command -v updater && command -v oras && command -v kubectl && command -v mc-monitor && command -v tar \
    && tar --version && oras version && kubectl version --client=true && mc-monitor version
FROM scratch
COPY --from=tools /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=verify /usr/local/bin/ /usr/local/bin/
COPY --from=toolbox /bin/busybox /bin/busybox
COPY --from=build /out/updater /usr/local/bin/updater
ENV PATH=/usr/local/bin:/bin HOME=/data
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/updater"]
CMD ["ready-check"]
