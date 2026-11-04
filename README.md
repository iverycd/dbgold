# dbgold

数据库迁移工具：后端 Go + Gin，前端 Vue 3 + Vite + Arco Design。

生产发布采用统一 HTTP 入口，Go 服务同时提供前端页面、`/api/*` 和健康检查。默认端口为 `18089`。服务器部署可从内网访问，Windows 单目录包默认只接受本机连接。

```text
http://服务器IP:18089/
http://服务器IP:18089/api/health/ready
```

正式用户流量应通过 HTTPS 网关进入；直接 HTTP 端口仅用于受控内网访问和检查。

## 单目录离线包（普通账号）

Linux x86_64 选 `portable-linux-amd64.tar.gz`，Linux aarch64 选 `portable-linux-arm64.tar.gz`，Windows x64 选 `portable-windows-amd64.zip`。将压缩包解压到自己可写、其他账号不能读取的目录。三个包的顶层目录都叫 `dbgold/`，包含程序、前端和神通 JDBC 驱动。普通账号可直接启动。只有连接神通数据库时才需要在目标机配置 Java。

| 平台 | 启动 | 浏览器访问 |
|---|---|---|
| Linux | 在解压目录运行 `./start.sh`，保持终端或 SSH 会话打开 | 默认监听 `0.0.0.0:18089`，在办公电脑打开终端列出的可达内网地址 |
| Windows | 双击 `start.cmd`，保持启动窗口打开 | 就绪后自动打开 `http://127.0.0.1:18089/` |

首次启动生成 `config/dbgold.env`，终端显示管理员 `admin` 的初始密码。可在该文件中修改 `LISTEN_HOST` 和 `PORT`（1024–65535），然后重启。Windows 自用模式默认仅监听本机。Linux 从办公电脑直连时，服务器防火墙需允许相应内网来源访问该端口。关闭浏览器后任务继续运行；结束迁移任务后可在启动窗口按 Ctrl+C 停止。

```text
dbgold/
  dbgold 或 dbgold.exe     主程序
  start.sh 或 start.cmd    前台启动入口
  web/                    前端静态资源
  lib/                    桥接 JAR、神通 JDBC 驱动与许可
  config/                 端口、密钥、管理员初始化配置
  data/                   dbgold.db，保存账号、连接和迁移历史
  uploads/                工单附件
  logs/                   日志
  tmp/                    数据校验等临时文件
  backups/                升级前的冷备份
```

路径以主程序所在目录为准。**搬移时先停机，再移动整个 `dbgold/` 目录。** 同一目录只能启动一个实例。静态站点只提供 `web/` 中的资源。

### 在目标机配置神通所需的 Java

原生包使用目标机的 Java 8 或更高版本，推荐 Java 17。首次使用神通时，dbgold 优先使用 `OSCAR_JAVA_BIN` 指定的可执行文件，其次使用 `JAVA_HOME` 下的 `bin/java`（Windows 为 `bin/java.exe`），最后在启动账号的 `PATH` 中查找。显式指定的路径无效时，神通连接会提示修正该路径。dbgold 的启动、登录、升级和其他数据库功能均无需 Java。

可在单目录包的 `config/dbgold.env` 中按目标机的实际安装位置填写一行。例如 Linux 使用 `OSCAR_JAVA_BIN=/opt/jdk-17/bin/java`，Windows 使用 `OSCAR_JAVA_BIN="C:/Program Files/Java/jdk-17/bin/java.exe"`。修改配置或系统环境变量后，重新启动 dbgold，再执行神通连接测试。

### 单目录包升级

1. 确认没有正在运行的迁移任务，关闭旧实例。准备足够空间容纳程序、配置、SQLite、上传文件及日志的冷备份。
2. 将同平台新版解压到**独立的临时目录**。在新版目录中执行升级命令，`--target` 指向原安装目录。

```sh
# Linux
./dbgold portable-upgrade --target "/data/迁移工具/dbgold" --confirm-no-running-tasks
```

```powershell
# Windows PowerShell
.\dbgold.exe portable-upgrade --target 'D:\迁移工具\dbgold' --confirm-no-running-tasks
```

升级器校验新包，在旧目录的 `backups/` 下建立冷备份，然后替换程序和资源，启动目标程序验活。成功后从原目录启动。若验活失败，升级器从同一份备份恢复旧程序及数据；若升级进程被意外中断，从新包重新执行原命令，先恢复快照。旧备份和临时文件不递归进入快照。

从旧版升级时，原有 `runtime/` 目录保留在原位。要继续使用其中的 Java，请将 `OSCAR_JAVA_BIN` 显式指向该目录里的可执行文件。

### 构建与目标机验收

```sh
GO_BIN=/Users/kay/sdk/go1.25.5/bin/go ./release.sh --portable v1.2.3
```

构建机使用 Go 1.25.5、Node/npm、JDK 和归档工具。JDK 用于编译和验证随包发布的桥接 JAR。输出在 `release/portable/v1.2.3/`，包含三个压缩包、`SHA256SUMS` 和版本清单。每个压缩包另含文件级 `manifest.sha256`。发布前须确认神通 JDBC 再分发许可，并提供 `third_party/oscar/LICENSE` 与指定校验值的驱动。

可在目标系统用普通账号运行 `python3 deploy/tests/smoke_portable.py <刚解压的dbgold目录>`。脚本在临时副本中验证启动登录、附件路径、整目录搬移、端口冲突回退及升级后的历史记录，需本机端口 18089 可用。配置好系统 Java 后，加 `--check-oscar-java` 可验证 dbgold 能启动外部 Java 并加载神通驱动。现场验收时，连接实际神通测试库并迁移一张含主键及中文文本的小表，核对目标行数和字段值。

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

## 测试/开发服务器一键部署

`deploy.sh` 用于把当前工作区快速部署到可直接运行 Linux/amd64 二进制的测试或开发服务器。每次部署都会构建后端、前端和 Oscar Java 桥接程序，并一起发布 JDBC 驱动及存在的许可文件，避免新后端仍加载旧桥接 JAR。

本机需要 Go 1.25.5、Node/npm、`sshpass`（macOS：`brew install sshpass`）及包含 `javac`、`jar` 的 JDK 9 或更高版本，前端依赖需提前安装。`GO_BIN` 默认为 `/Users/kay/sdk/go1.25.5/bin/go`，可通过环境变量覆盖。本机还必须有 `third_party/oscar/oscarJDBC8.jar` 和 `third_party/oscar/README.md`。`third_party/oscar/LICENSE` 为可选文件：存在时随包上传并校验，缺失不影响测试环境部署，也不会删除远程已有的许可文件。

远程服务器需要允许密码 SSH 登录，提供 Java 8 或更高版本及 `sha256sum`、`readlink`、`pgrep`、`nohup`、`timeout` 等 Linux 工具。使用 `-j` 指定远程 Java 的绝对路径；未指定时依次查找 `<部署目录>/runtime/bin/java` 和远程 `PATH` 中的 `java`。脚本不会自动安装 Java。

```bash
./deploy.sh -h <主机或IP> -u <远程账号> -p <远程密码> [-P <SSH端口>] [-d <部署目录>] [-r <服务端口>] [-j <远程Java绝对路径>]
```

- `-h`：远程主机或 IP，必填。
- `-u`：远程账号，必填。
- `-p`：远程密码，必填。
- `-P`：SSH 端口，默认 `22`。
- `-d`：远程部署目录，默认 `/opt/dbgold`。
- `-r`：远程 dbgold 服务端口，默认 `18089`。
- `-j`：远程 Java 可执行文件的绝对路径，可选，例如 `/usr/lib/jvm/java-17/bin/java`。

示例：

```bash
./deploy.sh -h 192.168.1.10 -u root -p 'secret' -P 22 -d /opt/dbgold -r 18089

# 使用其他本机 Go 路径，并指定远程 Java
GO_BIN=/path/to/go ./deploy.sh -h 192.168.1.10 -u root -p 'secret' -j /usr/lib/jvm/java-17/bin/java
```

脚本先把全部产物上传到远程暂存目录，校验 SHA-256，并检查所选 Java 能加载桥接程序和 JDBC 驱动；构建、上传或这些预检失败时，旧服务继续运行。全部通过后才停止本部署目录对应的 dbgold 实例，最多等待 20 秒优雅退出，再替换产物并启动新进程。

启动时显式设置 `STATIC_DIR`、`OSCAR_JAVA_BIN`、`OSCAR_BRIDGE_JAR`、`OSCAR_JDBC_JAR` 的绝对路径，确保加载本次发布的前端和 JAR，覆盖 `.env` 中可能遗留的旧路径。脚本会在最多 30 秒内检查新进程和 `/api/health/ready`，通过后才报告部署完成；失败时输出启动日志并返回非零状态。

远程 `dbgold.db`、`.env`、上传文件及应用日志会保留。部署完成后，可通过 `http://服务器IP:服务端口/` 访问服务；远程启动日志位于 `<部署目录>/dbgold.out`，应用日志默认位于 `<部署目录>/log/`。

可执行 `python3 deploy/tests/test_deploy.py` 验证部署流程；测试使用模拟命令，不会连接真实服务器或终止真实进程。

> 注意：脚本会通过命令行传递密码，密码可能短暂出现在本机进程列表中；同时为便于首次连接，脚本使用 `StrictHostKeyChecking=no` 放宽 SSH 主机密钥校验。此方式仅适用于直接运行二进制的测试/开发服务器，不能替代下文的 Docker 容器化生产安装或升级流程。

## 生成离线发布包

开发机需要 Go 1.25.5、Node/npm、JDK、Docker Buildx、zip、tar。版本化发布默认要求 Git 工作区干净：

```bash
GO_BIN=/Users/kay/sdk/go1.25.5/bin/go ./release.sh v1.2.3
```

构建机会访问 npm、Go 模块代理和基础镜像仓库；目标服务器无需联网。网络访问 `proxy.golang.org` 较慢时，可显式指定 Go 模块代理：

```bash
GOPROXY=https://goproxy.cn,direct GO_BIN=/Users/kay/sdk/go1.25.5/bin/go ./release.sh v1.2.3
```

脚本失败时默认清理临时文件；需要保留现场排查时，可增加 `KEEP_WORK_DIR=1`。开发阶段允许已跟踪文件未提交时，可使用 `ALLOW_DIRTY=1`，此时 manifest 的提交号会带 `-dirty` 标记；正式发布仍应在干净工作区执行。

输出位于 `release/v1.2.3/`：

- `dbgold-v1.2.3-linux-amd64.tar.gz`
- `dbgold-v1.2.3-linux-arm64.tar.gz`
- `dbgold-v1.2.3-windows-amd64.zip`
- `SHA256SUMS`
- `release-manifest.json`（版本、提交、构建时间和平台清单）

三个压缩包解压后都会生成一个与压缩包同名的外层目录。Linux 镜像按架构分别导出为 Docker archive，目标服务器不需要访问镜像仓库，也不需要安装 Docker Compose。

## Linux 安装

目标服务器只需要已启动且可访问的 Docker Engine。Docker 镜像包含神通所需的 Java，宿主机无需单独配置。选择与 `uname -m` 对应的发布包，解压并进入生成的目录后执行：

```bash
tar -xzf dbgold-v1.2.3-linux-amd64.tar.gz
cd dbgold-v1.2.3-linux-amd64
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

Linux 容器固定使用 Docker host 网络，应用直接在宿主机监听 `0.0.0.0:PORT`，没有 Docker 端口映射层。容器以非 root 用户运行，根文件系统只读，仅 `data`、`uploads`、`logs` 可写。安装脚本首次运行时生成 JWT 密钥和管理员密码，并只显示一次初始密码。

### 修改端口

推荐使用辅助命令，它会检查端口、更新配置、重建容器并验活：

```bash
sudo /opt/dbgold/set-port.sh --port 19089 --allow-cidr 192.168.1.0/24
```

修改端口必须使用 `set-port.sh`，它会同步修改配置、重建 host 网络容器并检查健康状态。还需要同步调整 firewalld、UFW 或云安全组规则。

### 容器管理

```bash
sudo docker ps --filter name=dbgold
sudo docker logs --tail 100 dbgold
sudo docker stop dbgold
sudo docker start dbgold
sudo docker restart dbgold
```

`docker restart` 不会重新读取环境文件；配置或端口发生变化时应使用部署辅助脚本重建容器。

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

解压 Windows 发布包后会得到同名外层目录。进入该目录，在管理员 PowerShell 中执行：

```powershell
Expand-Archive .\dbgold-v1.2.3-windows-amd64.zip -DestinationPath .
Set-Location .\dbgold-v1.2.3-windows-amd64
Set-ExecutionPolicy -Scope Process Bypass
.\install.ps1 -Port 18089
```

程序安装到 `C:\Program Files\dbgold`，数据保存到 `C:\ProgramData\dbgold`，并以低权限账户注册为自动启动的 Windows 服务。安装程序添加仅允许 Domain/Private 本地子网访问该程序的防火墙规则。

Windows 服务包也使用目标机 Java。使用神通时，建议在 `C:\ProgramData\dbgold\config\dbgold.env` 中填写 `OSCAR_JAVA_BIN="C:/Program Files/Java/jdk-17/bin/java.exe"`，替换为实际绝对路径，确保服务账户 `LocalService` 有读取和执行权限，再重启服务。通过环境变量配置时，使用该服务进程可见的系统变量。

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

## Nginx配置

```yaml
    server {
        listen 81;
        server_name  _;
        client_max_body_size 0;
        
        location / {
            try_files $uri $uri/ /index.html;
            root /opt/dbgold/dist;
            index index.html index.htm;
        }
        location /api/ {
            # Disable buffering for SSE
            #proxy_buffering off;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            # 推荐增加这一行，它会记录完整的代理链路 IP 列表 同时后端的.env需要增加TRUSTED_PROXIES=127.0.0.1,::1
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for; 
            #proxy_set_header REMOTE-HOST $remote_addr;
            proxy_pass http://127.0.0.1:8082/api/;
        }

        

        error_page 404 /404.html;
        location = /404.html {
        }

        error_page 500 502 503 504 /50x.html;
        location = /50x.html {
        }
    }
```
