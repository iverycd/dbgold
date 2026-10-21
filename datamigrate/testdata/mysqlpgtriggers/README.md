# MySQL → PG 内核触发器迁移

普通全量、仅结构任务可勾选“迁移触发器”，默认关闭。仅对象任务可选择“触发器”。仅数据任务及 CDC 不安装业务触发器，并在写入前检查目标，防止重复执行源端已经产生的副作用。CDC 每批事务也会检查，避免运行中安装触发器后重复回放。

## 转换边界

支持 BEFORE/AFTER INSERT/UPDATE/DELETE、SET NEW、BEGIN/END、IF/ELSEIF/ELSE、显式列的 INSERT VALUES、带 WHERE 的受限单表 UPDATE/DELETE。v2 另支持显式目标列、单张读取表、明确表达式和 WHERE 的 INSERT SELECT（可返回零行或多行）；不支持表别名、星号、JOIN、聚合、UNION、子查询、排序或 LIMIT。读取源 database 限定名只在与当前源库相同时映射为目标 schema。

表达式接受明确类型的常量、NEW/OLD、数值运算与比较、NULL 判断及布尔组合。同类型 IFNULL/COALESCE、直接写入时间列的 NOW/CURRENT_TIMESTAMP（精度 0 或 6）受支持；保存的 SQL_MODE 和时间列精度参与转换。CONCAT 支持文本、NULL、直接整数和定点数列、数值常量及嵌套 CONCAT，显式转为文本后用 `||` 拼接，任一参数为 NULL 则结果为 NULL；不接受浮点、时间、二进制及格式不明确的复合数值表达式。直接数值列还需冻结源 COLUMN_TYPE，拒绝 ZEROFILL 和与源 precision/scale 不符的格式。v2 文本 IFNULL/COALESCE 先逐参数转 text，保留 CHAR 为 NULL 时备用文本的尾空格；文本转换计划安装前还会检查空串、NULL 拼接和文本转换语义，不兼容的目标模式明确失败。

v2 对官方 MySQL 5.7/8.0/8.4 的 `utf8mb4_general_ci` 支持 `=`、`!=`/`<>`，要求参与比较的列排序规则一致。通过可选 Reader 能力读取原始列名和排序规则，分块读取实际 BMP 权重并验证 PAD SPACE、补充字符、NULL 等规则；仅将本对象使用的字符快照冻结在计划中，不修改通用表结构快照。目标使用内联字符映射及 UTF8 字节等值比较，不依赖目标默认 collation、ICU 或扩展。目标必须为 UTF8 并通过所需字符串函数的只读能力检查。

目标函数采用 SECURITY INVOKER，不映射 MySQL DEFINER。时间函数使用语句起始时间和目标业务会话时区，两端业务连接应保持相同时区。触发器外的普通表默认值、ON UPDATE 等既有迁移语义不由本功能扩展。

未知函数、过程调用、循环、变量、动态 SQL、复杂 DML、跨 database 引用、其他排序规则或文本大小排序、隐式类型转换、时间比较、执行顺序依赖，以及 BEFORE INSERT 读取自增列等，均报告为不支持自动转换。INSERT 遗漏动态默认列、UPDATE 遗漏 ON UPDATE 列也需人工适配；可以在源触发器中显式赋予受支持的表达式。同表/同事件/同时机多个触发器整组不安装。

一个逻辑触发器包含函数和触发器两个执行单元，整体事务安装，历史数据及行数验证完成后才安装。单个失败不停止其他对象。报告保留源定义与具体原因；原生成 SQL 可以整体只读重试，未知提交、结构/映射改变、数据依赖尚未完成时禁止重放。现有对象不会被覆盖或接管。

缺省版本及 v1 计划仍按原生成器校验，不升级已有冻结 SQL。原报告中“不支持自动转换”且没有计划或执行记录的对象，可以在修复页“查看诊断 → 重新生成触发器方案”中预览两个只读执行单元，核对保存后再确认执行。预览不写入修复记录、不执行目标 SQL；保存重新读取源定义并校验对象、连接绑定、依赖表版本和方案摘要。原源定义改变时拒绝替换，原报告保留。

字符串比较键会对参与比较的值执行字符映射，查询条件通常不能直接使用原文本列的普通索引。此次不自动建索引；对大表或高频写入，需按实际目标版本检查业务执行计划和吞吐。本测试记录两个查询匹配行的小型夹具耗时，包含连接往返，不能作为生产容量承诺。

2026-10-21 最终回归在同时运行的隔离容器中记录：每组 13 次目标更新、2 行查询表，PostgreSQL 平均 2.31–3.54 ms/次，openGauss PG 模式平均 3.02 ms/次。此值包含触发器判断、可能的查询/审计插入及网络往返；不同输入并非每次都产生审计。大表性能尚未由该夹具验证。

## 可重复的实库测试

设置 `DBGOLD_MYSQLPG_TRIGGER_SOURCE_DSN`（MySQL 管理连接，可不指定 database）和 `DBGOLD_MYSQLPG_TRIGGER_TARGET_DSN`。测试仅在随机创建的 `dbgold_trg_it_*` database/schema 中创建、读取、删除对象，并清理；账号需具备相应权限。不会修改服务器全局参数。

`DBGOLD_MYSQLPG_TRIGGER_PRODUCT` 默认为 `postgres`，其他可选值为 `gaussdb`、`highgo`、`vastbase`、`gbase`、`seabox`、`kingbase`，分别使用项目真实 Writer 和驱动。

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestMySQLPGTriggerIntegration$' -count=1 -v
```

覆盖六类事件以及提供的 `update_enableuser`、`update_isepoint` 两条业务触发器，共八个对象，比较源/目标数据和审计副作用。验证条件、NULL、历史数据只复制一次、多行 INSERT、汇总、DELETE、NOW 精度、失败回滚、INSERT SELECT 零行/多行及字符串大小写、重音、尾空格、`ß/s/ss`、组合字符、emoji。另有目标层 `TestMySQLPGTriggerSQLIntegration`，通过 `DBGOLD_TEST_POSTGRES_DSN`、`DBGOLD_TEST_GAUSSDB_DSN` 等七个产品 DSN 验证中文/双引号命名空间、已有对象冲突、冻结计划严格复用及补迁保护。未配置实库时测试明确跳过，不代表已验证。

## 实测记录（2026-10-21）

| 源 | 目标 | 结果 |
| --- | --- | --- |
| MySQL 5.7.20 | PostgreSQL 16.15 | 八个触发器全流程行为通过，包含两条业务 SQL 及默认 SQL_MODE |
| MySQL 8.0.36 | PostgreSQL 16.15 | 八个触发器全流程行为通过，包含两条业务 SQL |
| MySQL 8.4.11 | PostgreSQL 16.15 | 八个触发器全流程行为通过；目标严格复用及命名空间测试通过 |
| MySQL 8.4.11 | openGauss-lite 5.0.3，PG 模式，GaussDB Writer 路径 | 八个触发器全流程行为通过；目标严格复用及命名空间测试通过；不等同于商业 GaussDB 认证 |
| 商业 GaussDB、瀚高、海量、GBase、海云、人大金仓 | 对应实际版本与兼容模式 | 已接入共享实现，缺对应实库，尚未验证 |

openGauss-lite 5.0.3 的本次环境未提供可用 UUID 函数，现有 `get_uuid` 初始化失败；既有 `changedatatohistory`、`changedatatomain` 的能力探测也因 `ON COMMIT DROP` 不支持而失败。这些独立兼容项会影响整体任务状态，触发器行为测试保留并输出这些诊断，不将其报告为完整任务全部成功。
