# dbgold

数据库迁移工具:后端 Go + Gin,前端 Vue 3 + Vite + Arco Design。

## 部署

`deploy.sh` 一键完成本地编译、前端打包、远程拷贝与重启。

### 前置条件

- 本机已安装 [`sshpass`](https://gitlab.com/jeffdyck/sshpass)(macOS:`brew install sshpass`)
- 本机 Go 工具链位于 `/Users/kay/sdk/go1.25.5/bin/go`
- 远程为 Linux(amd64),`dist` 前端静态文件由远程 nginx 托管(默认指向 `<部署目录>/dist`)
- 远程账号对部署目录(默认 `/opt/dbgold`)有写权限

### 用法

```bash
./deploy.sh -h <host> -u <user> -p <password> [-P <ssh端口>] [-d <部署目录>] [-r <服务端口>]
```

| 参数 | 含义 | 必填 | 默认 |
|------|------|------|------|
| `-h` | 远程主机 host/IP | 是 | — |
| `-u` | 远程账号 | 是 | — |
| `-p` | 远程密码 | 是 | — |
| `-P` | SSH 端口 | 否 | `22` |
| `-d` | 远程部署目录 | 否 | `/opt/dbgold` |
| `-r` | 远程 dbgold 监听端口(传给 `PORT` 环境变量) | 否 | `8080` |

### 示例

```bash
./deploy.sh -h 192.168.1.10 -u root -p 'secret' -P 22 -d /opt/dbgold -r 8080
```

### 执行流程

1. 后端交叉编译:`GOOS=linux GOARCH=amd64 go build -o dbgold dbgold`
2. 前端打包:`frontend` 目录下 `npm run build` 生成 `dist`
3. 远程创建部署目录
4. 停止远程旧进程:`pgrep -x dbgold` 命中则 `kill`,等待最多 5 秒,仍存活则 `kill -9`
5. 拷贝产物:`dbgold` 二进制与 `dist` 到部署目录(拷贝前清理旧文件)
6. 远程启动:`PORT=<服务端口> nohup <部署目录>/dbgold &`,启动后校验进程存活

### 说明

- **不拷贝 `dbgold.db`**:它是运行时数据库,保留远程已有数据。库文件由 dbgold 首次启动时在部署目录下自动生成。
- **dist 由 nginx 托管**:后端不提供静态文件服务,`dist` 仅拷贝到部署目录,需 nginx 指向该目录。
- 默认放宽 SSH host key 校验(`StrictHostKeyChecking=no`),便于首次连接。
- 使用 `sshpass -p` 传密码,密码会短暂出现在本机进程列表中。
- 运行日志:标准输出在 `<部署目录>/dbgold.out`,应用日志在 `<部署目录>/log/`。

### 远程环境变量

后端通过环境变量配置,脚本仅设置 `PORT`,其余使用代码内置默认值:

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `8080` | 监听端口(由 `-r` 指定) |
| `SQLITE_PATH` | `dbgold.db` | SQLite 路径(相对启动目录) |
| `JWT_SECRET` | `change-me-in-production` | JWT 签名密钥 |
| `ADMIN_USER` | `admin` | 初始管理员用户名 |
| `ADMIN_PASS` | `Admin@123` | 初始管理员密码 |

> 生产环境建议在部署目录放置启动脚本覆盖 `JWT_SECRET`、`ADMIN_PASS` 等敏感默认值。
