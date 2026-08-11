FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY static ./static
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /quadlet-manager .

# systemctl/journalctl 需要通过 D-Bus 与宿主机 systemd 通信，
# 所以最终镜像用 debian 并安装 systemd 提供这两个客户端工具。
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends systemd \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /quadlet-manager /usr/local/bin/quadlet-manager
ENV QM_QUADLET_DIR=/etc/containers/systemd \
    QM_LISTEN=0.0.0.0:8600
EXPOSE 8600
ENTRYPOINT ["/usr/local/bin/quadlet-manager"]
