# dbgold

数据库迁移工具：后端 Go + Gin，前端 Vue 3 + Vite + Arco Design。

生产发布采用统一 HTTP 入口，Go 服务同时提供前端页面、`/api/*` 和健康检查。部署默认监听所有 IPv4 网卡的 `18089` 端口：

```text
http://服务器IP:18089/
http://服务器IP:18089/api/health/ready
```

正式用户流量应通过 HTTPS 网关进入；直接 HTTP 端口仅用于受控内网访问和检查。

## 本地开发

后端默认监听 `0.0.0.0:18089`：

```bash
go run .
```

也可以显式指定配置和端口：

```bash
go run . serve --config .env --listen-host 0.0.0.0 --port 19089
```

前端开发服务器仍使用 Vite；它会把 `/api` 代理到 `VITE_API_TARGET`，未设置时为 `http://localhost:18089`。需要切换后端端口时，在 `frontend/.env.local` 中设置：

```dotenv
VITE_API_TARGET=http://localhost:18089
```

生产构建中，后端直接托管 `frontend/dist`，不再要求目标服务器单独安装 nginx 来提供静态文件。

## 生成离线发布包

开发机需要 Go 1.25.5、Node/npm、Docker Buildx、zip、tar。版本化发布默认要求 Git 工作区干净：

```bash
GO_BIN=/Users/kay/sdk/go1.25.5/bin/go ./release.sh v1.2.3
```

输出位于 `release/v1.2.3/`：

- `dbgold-v1.2.3-linux-amd64.tar.gz`
- `dbgold-v1.2.3-linux-arm64.tar.gz`
- `dbgold-v1.2.3-windows-amd64.zip`
- `SHA256SUMS`
- `release-manifest.json`（版本、提交、构建时间和平台清单）

Linux 镜像按架构分别导出为 Docker archive，目标服务器不需要访问镜像仓库。

## Linux 安装

目标服务器需要 Docker Engine 和 Docker Compose v2。选择与 `uname -m` 对应的发布包，解压后执行：

```bash
sudo ./install.sh --port 18089 --allow-cidr 192.168.1.0/24
```

默认目录：

```text
/opt/dbgold/config/dbgold.env
/opt/dbgold/data/
/opt/dbgold/uploads/
/opt/dbgold/logs/
/opt/dbgold/backups/
```

容器以非 root 用户运行，根文件系统只读，仅 `data`、`uploads`、`logs` 可写。安装脚本首次运行时生成 JWT 密钥和管理员密码，并只显示一次初始密码。

### 修改端口

推荐使用辅助命令，它会检查端口、更新配置、重建容器并验活：

```bash
sudo /opt/dbgold/set-port.sh --port 19089 --allow-cidr 192.168.1.0/24
```

也可以编辑 `/opt/dbgold/config/dbgold.env` 中的 `PORT`，然后执行：

```bash
cd /opt/dbgold
sudo docker compose --env-file config/dbgold.env -f compose.yaml up -d --force-recreate
```

Compose 必须带 `--env-file`，这样宿主机映射和容器监听端口才会同步变化。

### 备份与升级

备份会短暂停止服务，确保 SQLite 一致：

```bash
sudo /opt/dbgold/backup.sh
```

升级前在界面确认没有运行中的迁移任务，然后在新发布包目录执行：

```bash
sudo ./upgrade.sh --confirm-no-running-tasks
```

升级失败时脚本恢复旧镜像和冷备份。手工恢复必须明确确认：

```bash
sudo /opt/dbgold/restore.sh --backup /opt/dbgold/backups/dbgold-时间.tar.gz --yes
```

## Windows x64 安装

解压 Windows 发布包，在管理员 PowerShell 中执行：

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\install.ps1 -Port 18089
```

程序安装到 `C:\Program Files\dbgold`，数据保存到 `C:\ProgramData\dbgold`，并以低权限账户注册为自动启动的 Windows 服务。安装程序添加仅允许 Domain/Private 本地子网访问该程序的防火墙规则。

修改端口：

```powershell
& 'C:\Program Files\dbgold\set-port.ps1' -Port 19089
```

配置也可在 `C:\ProgramData\dbgold\config\dbgold.env` 中修改，修改后执行：

```powershell
Restart-Service dbgold
```

前台诊断命令：

```powershell
& 'C:\Program Files\dbgold\dbgold.exe' serve `
  --listen-host 0.0.0.0 --port 19089 `
  --config 'C:\ProgramData\dbgold\config\dbgold.env'
```

升级：

```powershell
.\upgrade.ps1 -ConfirmNoRunningTasks
```

Windows 冷备份使用目录快照，不受 ZIP 单文件大小限制：

```powershell
& 'C:\Program Files\dbgold\backup.ps1'
& 'C:\Program Files\dbgold\restore.ps1' -Backup 'C:\ProgramData\dbgold\backups\dbgold-时间' -ConfirmRestore
```

卸载服务并保留运行数据：

```powershell
& 'C:\Program Files\dbgold\uninstall.ps1'
```

## 健康检查和 HTTPS 网关

```bash
dbgold healthcheck --url http://127.0.0.1:18089/api/health/ready
```

- `/api/health/live` 仅表示进程存活。
- `/api/health/ready` 同时检查 SQLite 和前端构建产物。

外部网关需要把整个站点代理到 `http://dbgold内网IP:18089`。SSE 路径必须关闭代理缓冲，上传限制和超时必须覆盖 `MAX_UPLOAD_BYTES`；参考 `deploy/nginx/dbgold.conf.example`。把网关 IP 或 CIDR 写入 `TRUSTED_PROXIES`，多个值用逗号分隔。

## 生产配置

真实环境变量优先于配置文件，命令行的 `--listen-host`、`--port` 又优先于两者。主要配置：

| 变量 | 部署默认值 | 说明 |
|---|---|---|
| `APP_ENV` | `production` | 生产模式会拒绝不安全的默认密钥和密码 |
| `LISTEN_HOST` | `0.0.0.0` | 监听所有 IPv4 网卡 |
| `PORT` | `18089` | HTTP 统一入口，范围 `1024–65535` |
| `STATIC_DIR` | 平台相关绝对路径 | 前端 `web` 目录 |
| `TRUSTED_PROXIES` | 空 | HTTPS 网关 IP/CIDR |
| `SQLITE_PATH` | 平台相关绝对路径 | SQLite 数据库 |
| `UPLOAD_DIR` | 平台相关绝对路径 | 工单上传目录 |
| `LOG_DIR` | 平台相关绝对路径 | 应用日志目录 |

本方案为单机单实例部署。升级必须使用维护窗口，不支持多副本或无停机切换。
