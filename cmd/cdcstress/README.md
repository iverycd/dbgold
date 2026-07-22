# MySQL → GaussDB CDC 压力测试套件

`cdcstress` 通过真实的 dbgold 增量迁移 API 启动和管理 CDC 任务，同时直连隔离的测试命名空间进行造数、施压和逐表校验。

工具不会从 JSON 配置文件读取密码，也不会在日志中输出密码。

## 安全机制

- MySQL database 和 GaussDB schema 必须命名为 `dbgold_cdc_stress`，或以 `dbgold_cdc_stress_` 开头。
- 自动生成的表统一使用 `cs_<run-token>_` 前缀，并且在创建前写入本次运行的对象清单。
- 当运行状态中仍记录着活动任务时，`cleanup` 会拒绝执行。
- 清理前会检查对象前缀，并要求 `--run-id` 和 `--confirm-run-id` 完全一致。
- `cleanup` 会删除专用的 MySQL database、跨库噪声 database 和 GaussDB schema。禁止将本工具指向包含需要保留数据的命名空间。
- 工具不会自动停止或重启 dbgold、MySQL、GaussDB，也不会修改网络配置。

## 配置和密码

复制 [cdcstress.example.json](./cdcstress.example.json) 到不受版本控制的路径，然后设置以下环境变量：

```bash
export CDCSTRESS_MYSQL_PASSWORD='...'
export CDCSTRESS_GAUSSDB_PASSWORD='...'

# 方式一：使用已有 Token
export CDCSTRESS_DBGOLD_TOKEN='...'

# 方式二：由工具登录 dbgold
export CDCSTRESS_DBGOLD_USERNAME='...'
export CDCSTRESS_DBGOLD_PASSWORD='...'
```

dbgold 中配置的源连接和目标连接，必须与 JSON 中的直连数据库地址保持一致：

- MySQL 主机和端口必须一致。dbgold 连接中保存的 database 可以不同，因为 CDC API 会显式选择压力测试 database。
- GaussDB 主机、端口和 database 必须完全一致。
- 可以通过连接 ID 或精确的连接名称定位连接，两种方式只能配置一种。

## 内置规模档位

| 档位 | 表数量 | 初始总行数 |
|---|---:|---:|
| `small` | 100 | 1,000,000 |
| `medium` | 1,000 | 10,000,000 |
| `large` | 3,000 | 30,000,000 |

如需更小的冒烟测试或模拟具体生产画像，将 `profile.name` 设置为 `custom`，并同时提供 `table_count` 和 `total_rows`。

建议按 `small` → `medium` → `large` 的顺序执行，只有前一级报告通过后再进入下一级。

## 命令说明

### 1. 执行只读预检

检查数据库连接、MySQL binlog 配置、账号权限，以及 dbgold 中的连接配置是否与直连配置一致。

```bash
go run ./cmd/cdcstress precheck -config /path/to/cdcstress.json
```

### 2. 创建测试数据

```bash
go run ./cmd/cdcstress prepare -config /path/to/cdcstress.json
```

命令会输出本次运行的 `run_id`，后续命令必须使用同一个 ID。

默认只在 MySQL 中造数，并创建空的 GaussDB 测试 schema。如果要直接测试全新的 `incremental_only` 任务，需要增加 `-target`，同时创建并填充对应的 GaussDB 表：

```bash
go run ./cmd/cdcstress prepare -config /path/to/cdcstress.json -target
```

使用 `-mode both` 时不需要 `-target`：工具会先执行全量快照加 CDC，再复用已经同步完成的目标表测试指定点增量模式。

### 3. 运行 CDC 场景

```bash
go run ./cmd/cdcstress run \
  -config /path/to/cdcstress.json \
  -run-id run_... \
  -mode both
```

`-mode` 支持：

- `full_then_cdc`：全量快照后持续同步。
- `incremental_only`：从当前 GTID 或 binlog 文件位点开始同步。
- `both`：依次执行以上两种模式，默认值。

增加 `-manual-restart` 可以覆盖应用重启场景：

```bash
go run ./cmd/cdcstress run \
  -config /path/to/cdcstress.json \
  -run-id run_... \
  -mode both \
  -manual-restart
```

工具运行到重启检查点后会暂停并提示操作人员安全重启 dbgold。确认 dbgold 已恢复后按回车，工具会检查任务状态并从 checkpoint 恢复。

如果某个高 TPS 档位触及容量边界后需要从该档位继续诊断，可使用 `-start-tps` 跳过更低档位，而不修改配置文件或运行状态哈希：

```bash
go run ./cmd/cdcstress run \
  -config /path/to/cdcstress.json \
  -run-id run_... \
  -mode both \
  -start-tps 1000
```

### 4. 单独执行数据校验

```bash
go run ./cmd/cdcstress verify \
  -config /path/to/cdcstress.json \
  -run-id run_...
```

校验内容包括：

- 所有迁移表的源端和目标端行数。
- 有主键或唯一定位键表的有序完整摘要。
- 无主键表的行摘要多重集合，保留重复行数量。
- 同 database 非匹配表和其他 database 的噪声数据是否被错误同步。

### 5. 清理测试数据

```bash
go run ./cmd/cdcstress cleanup \
  -config /path/to/cdcstress.json \
  -run-id run_... \
  -confirm-run-id run_...
```

只有两个运行 ID 完全一致、对象清单安全且没有活动任务时才会执行清理。

## 自动化测试场景

一次完整运行包含以下场景：

1. 100、1000 或 3000 张表的大规模预检和定位策略识别。
2. 一致性全量快照期间持续写入源库。
3. 热点表和冷表偏斜分布的 INSERT、UPDATE、DELETE 混合流量。
4. 单行事务、跨表事务、长事务和显式回滚。
5. 主键、复合主键、唯一索引和无主键整行定位。
6. 主键、复合主键和唯一定位键发生变化时的同步。
7. 阶梯负载以及短时突发流量后的积压追赶。
8. 任务暂停期间继续写入，恢复后检查 checkpoint、重复和丢失。
9. 可选的 dbgold 人工重启及断点恢复。
10. MySQL DDL 导致 CDC 暂停、目标端结构修复、DDL 确认和元数据刷新。
11. 人工制造无主键目标行冲突，验证 checkpoint 不前移以及修复后的重放。
12. binlog 文件轮换、空闲等待以及恢复写入。
13. 同 database 表过滤和跨 database 事件隔离。
14. 停止写入、锁定 cutover 边界、追平、最终校验和安全停止。

暂停恢复场景会先完成配置时长的源端写入，然后立即恢复 CDC。暂停期间选取的延迟采样标记会写入 `state.json`，待 CDC 报告追平后再到目标端读取并计算延迟，因此不会因为 `catch_up_timeout` 为 30 分钟而在恢复前固定等待 30 分钟。该阶段的延迟包含暂停时间和恢复追赶时间。

## 负载和性能基线

默认 TPS 阶梯为：

```text
50 → 100 → 250 → 500 → 1000
```

每档默认运行 2 分钟，随后执行短时双倍 TPS 突发流量。所有参数均可在 JSON 的 `workload` 中调整。

工具会复用数据库连接，避免高 TPS 下因频繁建立 TCP 连接耗尽本机临时端口。连接池默认值为：

- 最大连接数和最大空闲连接数：`max(profile.workers, workload.workers) + 4`。
- 连接最大生命周期：30 分钟。
- 连接最大空闲时间：5 分钟。

如需按数据库连接限制调整，可在配置中增加可选的 `database_pool`：

```json
{
  "database_pool": {
    "max_open_conns": 12,
    "max_idle_conns": 12,
    "conn_max_lifetime": "30m",
    "conn_max_idle_time": "5m"
  }
}
```

首轮测试只建立基线，不设置硬性性能 SLA。报告会记录：

- 目标 TPS 和实际提交 TPS。
- 抽样端到端同步延迟 P50、P95、P99。
- 最大 CDC lag 和积压追赶时间。
- MySQL 连接线程、运行线程及 GaussDB 会话峰值。
- CDC 事件计数、任务状态和错误。

负载错误分为“数据库容量边界”“压测环境或连接故障”和“正确性错误”。`can't assign requested address`、连接拒绝、网络不可达或文件描述符耗尽属于压测环境故障，本次运行会失败，不会被记录成 MySQL 容量上限。

出现以下任一情况时，本次运行判定失败：

- 源端与目标端数据不一致。
- 非迁移范围的数据出现在目标端。
- CDC 任务发生未预期的暂停、失败或终止。
- DML 应用报错。
- 在配置的超时时间内无法追平。
- checkpoint 或冲突恢复流程不符合预期。

## 状态、账本和报告

运行数据默认保存在：

```text
cdcstress-results/<run-id>/
```

目录内容包括：

- `state.json`：可恢复的运行状态、任务 ID、对象清单和场景进度。
- `ledger.jsonl`：已提交和已回滚操作的事务账本。
- `report.json`：机器可读的完整测试报告。
- `report.md`：便于人工查看的 Markdown 报告。

如果 CLI 中断，再次使用相同配置和 `run_id` 执行 `run`。工具会检查已有活动任务，避免重复创建任务；对于安全可恢复的暂停或重启状态，会从目标 checkpoint 继续。

新版状态文件会在每个 TPS、突发、暂停恢复、DDL、冲突和切换阶段结束后立即保存结果，续跑时不会重复已经完成的阶段。

旧版状态文件没有阶段级记录，不能自动判断控制台中已经执行过哪些负载。仅针对这种旧状态，可通过 `-resume-from pause-resume` 明确从暂停恢复场景继续；此前阶段会在报告中标为“旧版本运行、指标未持久化”，不会伪造性能数据。当前示例运行可使用：

```bash
go run ./cmd/cdcstress run \
  -config /Users/kay/Downloads/cdcstress.json \
  -run-id run_20260722T062114Z \
  -mode both \
  -resume-from pause-resume
```

保留多次运行的数据时，应为每次运行使用不同的 MySQL database 和 GaussDB schema；否则应先清理上一轮数据。

## 真实数据库集成测试

集成测试会真实创建和删除 database、schema、表及数据，因此同时使用 build tag 和环境变量进行保护。

请使用不超过 20 张表、100,000 行的 `custom` 配置执行：

```bash
CDCSTRESS_INTEGRATION=1 \
CDCSTRESS_CONFIG=/path/to/small-integration.json \
CDCSTRESS_INTEGRATION_CLEANUP=1 \
go test -tags=integration ./cmd/cdcstress \
  -run TestCDCStressIntegration -v -count=1
```

只有设置 `CDCSTRESS_INTEGRATION_CLEANUP=1`，并且完整测试成功后，集成测试才会执行清理。测试失败时会保留现场，便于排查。
