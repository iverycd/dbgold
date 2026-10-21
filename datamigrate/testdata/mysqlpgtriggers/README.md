# MySQL → PG 内核触发器迁移

普通全量、仅结构任务可勾选“迁移触发器”，默认关闭。仅对象任务可选择“触发器”。仅数据任务及 CDC 不安装业务触发器，并在写入前检查目标，防止重复执行源端已经产生的副作用。CDC 每批事务也会检查，避免运行中安装触发器后重复回放。

## 转换边界

支持 BEFORE/AFTER INSERT/UPDATE/DELETE、SET NEW、BEGIN/END、IF/ELSEIF/ELSE、显式列的单行 INSERT VALUES、带 WHERE 的受限单表 UPDATE/DELETE。表达式仅接受明确类型的常量、NEW/OLD、数值运算与比较、NULL 判断及布尔组合。同类型 IFNULL/COALESCE、直接写入时间列的 NOW/CURRENT_TIMESTAMP（精度 0 或 6）受支持；保存的 SQL_MODE 和时间列精度参与转换。

目标函数采用 SECURITY INVOKER，不映射 MySQL DEFINER。时间函数使用语句起始时间和目标业务会话时区，两端业务连接应保持相同时区。触发器外的普通表默认值、ON UPDATE 等既有迁移语义不由本功能扩展。

未知函数、过程调用、循环、变量、动态 SQL、复杂 DML、跨 database 引用、依赖排序规则的文本比较、隐式类型转换、时间比较、执行顺序依赖，以及 BEFORE INSERT 读取自增列等，均报告为不支持自动转换。INSERT 遗漏动态默认列、UPDATE 遗漏 ON UPDATE 列也需人工适配；可以在源触发器中显式赋予受支持的表达式。同表/同事件/同时机多个触发器整组不安装。

一个逻辑触发器包含函数和触发器两个执行单元，整体事务安装，历史数据及行数验证完成后才安装。单个失败不停止其他对象。报告保留源定义与具体原因；原生成 SQL 可以整体只读重试，未知提交、结构/映射改变、数据依赖尚未完成时禁止重放。现有对象不会被覆盖或接管。

## 可重复的实库测试

设置 `DBGOLD_MYSQLPG_TRIGGER_SOURCE_DSN`（MySQL 管理连接，可不指定 database）和 `DBGOLD_MYSQLPG_TRIGGER_TARGET_DSN`。测试仅在随机创建的 `dbgold_trg_it_*` database/schema 中创建、读取、删除对象，并清理；账号需具备相应权限。不会修改服务器全局参数。

`DBGOLD_MYSQLPG_TRIGGER_PRODUCT` 默认为 `postgres`，其他可选值为 `gaussdb`、`highgo`、`vastbase`、`gbase`、`seabox`、`kingbase`，分别使用项目真实 Writer 和驱动。

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestMySQLPGTriggerIntegration$' -count=1 -v
```

覆盖六种触发器、条件与 NULL 赋值、历史审计数据只复制一次、多行 INSERT、汇总更新、DELETE、NOW 精度和失败行副作用回滚，并比较源/目标数据。另有目标层 `TestMySQLPGTriggerSQLIntegration`，通过 `DBGOLD_TEST_POSTGRES_DSN`、`DBGOLD_TEST_GAUSSDB_DSN` 等七个产品 DSN 验证中文/双引号命名空间、已有对象冲突、冻结计划严格复用及补迁保护。未配置实库时测试明确跳过，不代表已验证。

## 实测记录（2026-10-21）

| 源 | 目标 | 结果 |
| --- | --- | --- |
| MySQL 5.7.20 | PostgreSQL 16.15 | 六种触发器全流程行为通过，包含默认 SQL_MODE |
| MySQL 8.0.36 | PostgreSQL 16.15 | 六种触发器全流程行为通过 |
| MySQL 8.4.11 | PostgreSQL 16.15 | 六种触发器全流程行为、目标严格复用及命名空间测试通过 |
| MySQL 8.4.11 | openGauss-lite 5.0.3，PG 模式，GaussDB Writer 路径 | 六种触发器全流程行为、目标严格复用及命名空间测试通过；不等同于商业 GaussDB 认证 |
| 商业 GaussDB、瀚高、海量、GBase、海云、人大金仓 | 对应实际版本与兼容模式 | 已接入共享实现，缺对应实库，尚未验证 |

openGauss-lite 5.0.3 的本次环境未提供可用 UUID 函数，现有 `get_uuid` 初始化失败；既有 `changedatatohistory`、`changedatatomain` 的能力探测也因 `ON COMMIT DROP` 不支持而失败。这些独立兼容项会影响整体任务状态，触发器行为测试保留并输出这些诊断，不将其报告为完整任务全部成功。
