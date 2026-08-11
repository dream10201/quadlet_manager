# Quadlet Manager

单二进制的 Web 服务（Go，前端通过 `go:embed` 打包进 bin），用于管理 podman Quadlet `.container` 文件：

- 交互式依赖关系图（vis-network）：`Requires` / `Wants` / `BindsTo` / `PartOf` / `After` / `Before` 分色显示，箭头 A → B 表示 A 依赖 B；可选显示网络（`Network=`）分组。
- 节点颜色实时反映 systemd 状态（每 4 秒轮询）：绿=active，红=failed，灰=inactive，黄=切换中。
- 点击节点：查看状态/镜像/网络，start / stop / restart / enable / disable，查看 journal 日志。
- 在界面上直接添加/删除依赖：只做行级修改，保留文件中的注释和顺序，写回后自动 `systemctl daemon-reload`。
- 内置原始文件编辑器（保存前校验含 `[Container]` 段）。

## 构建 / 运行

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o quadlet-manager .
QM_QUADLET_DIR=/etc/containers/systemd ./quadlet-manager
# 打开 http://127.0.0.1:8600
```

环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QM_QUADLET_DIR` | `/etc/containers/systemd` | Quadlet 文件目录 |
| `QM_LISTEN` | `127.0.0.1:8600` | 监听地址 |
| `QM_USER_MODE` | `0` | 设为 `1` 时使用 `systemctl --user`（rootless，目录一般是 `~/.config/containers/systemd`） |

## 容器方式运行

CI（GitHub Actions）会把多架构镜像（amd64/arm64）推到 `ghcr.io/<owner>/quadlet-manager`。
容器内的 `systemctl`/`journalctl` 通过挂载的宿主机 D-Bus socket 和 journal 工作，
推荐直接用仓库里的 [quadlet-manager.container](quadlet-manager.container)（自己也被 Quadlet 管理），或等价的：

```bash
podman run -d --name quadlet-manager \
  -p 127.0.0.1:8600:8600 \
  -v /etc/containers/systemd:/etc/containers/systemd \
  -v /run/dbus/system_bus_socket:/run/dbus/system_bus_socket \
  -v /var/log/journal:/var/log/journal:ro \
  -v /etc/machine-id:/etc/machine-id:ro \
  ghcr.io/<owner>/quadlet-manager:latest
```

注意：容器方式只支持 system 模式（不支持 `QM_USER_MODE=1`）；rootless 场景请直接跑二进制。

## CI

`.github/workflows/build.yml`：

- push / PR：gofmt 检查、`go vet`、编译。
- push 到 main 或打 `v*` tag：buildx 构建 amd64+arm64 镜像推送 ghcr（main → `latest`，tag → 版本号）。
- `v*` tag：同时把两个架构的静态二进制传到 GitHub Release。

## 安全说明

- 服务能启停单元、改写 Quadlet 文件，**没有认证**，默认只监听 127.0.0.1。如需远程访问，请放在带认证的反向代理（如 caddy + oauth2-proxy）后面，不要直接暴露端口。
- 修改依赖后会自动 daemon-reload，但对运行中的容器要重启对应单元才生效。

## API 摘要

- `GET /api/graph` 全量图（单元、依赖边、网络）
- `GET /api/status` 仅状态（轮询用）
- `POST /api/units/<f>.container/action` `{"action":"start|stop|restart|enable|disable"}`
- `POST /api/units/<f>.container/deps` `{"op":"add|remove","directive":"Requires|...","target":"xxx.service"}`
- `GET|PUT /api/units/<f>.container/file` 读/写原始文件
- `GET /api/units/<f>.container/logs?lines=80` journal 日志
- `POST /api/daemon-reload`
