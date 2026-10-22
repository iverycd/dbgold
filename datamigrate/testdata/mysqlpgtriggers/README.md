# MySQL → PG 内核触发器迁移

普通全量、仅结构任务可勾选“迁移触发器”，默认关闭。仅对象任务可选择“触发器”。仅数据任务及 CDC 不安装业务触发器，并在写入前检查目标，防止重复执行源端已经产生的副作用。CDC 每批事务也会检查，避免运行中安装触发器后重复回放。

## 转换边界

支持 BEFORE/AFTER INSERT/UPDATE/DELETE、SET NEW、BEGIN/END、IF/ELSEIF/ELSE、显式列的 INSERT VALUES、带 WHERE 的受限单表 UPDATE/DELETE。v2 另支持显式目标列、单张读取表、明确表达式和 WHERE 的 INSERT SELECT（可返回零行或多行）；不支持表别名、星号、JOIN、聚合、UNION、子查询、排序或 LIMIT。读取源 database 限定名只在与当前源库相同时映射为目标 schema。

表达式接受明确类型的常量、NEW/OLD、数值运算与比较、NULL 判断及布尔组合。同类型 IFNULL/COALESCE、直接写入时间列的 NOW/CURRENT_TIMESTAMP（精度 0 或 6）受支持；保存的 SQL_MODE 和时间列精度参与转换。CONCAT 支持文本、NULL、直接整数和定点数列、数值常量及嵌套 CONCAT，显式转为文本后用 `||` 拼接，任一参数为 NULL 则结果为 NULL；不接受浮点、时间、二进制及格式不明确的复合数值表达式。直接数值列还需冻结源 COLUMN_TYPE，拒绝 ZEROFILL 和与源 precision/scale 不符的格式。v2 文本 IFNULL/COALESCE 先逐参数转 text，保留 CHAR 为 NULL 时备用文本的尾空格；文本转换计划安装前还会检查空串、NULL 拼接和文本转换语义，不兼容的目标模式明确失败。

v2 对 MySQL 5.7/8.0/8.4 的 `utf8mb4_general_ci` 支持 `=`、`!=`/`<>`，要求参与比较的列排序规则一致。允许标准 MySQL 标识和精确的 `Source distribution` 源码构建标识，继续排除已知分支，并验证实际排序行为。通过可选 Reader 能力读取原始列名和排序规则，分块读取实际 BMP 权重并验证 PAD SPACE、补充字符、NULL 等规则；仅将本对象使用的字符快照冻结在计划中，不修改通用表结构快照。目标使用内联字符映射及 UTF8 字节等值比较，不依赖目标默认 collation、ICU 或扩展。目标必须为 UTF8 并通过所需字符串函数的只读能力检查。

v4 对通过只读能力检查的 Kingbase MySQL 模式支持 `SET @变量 = 表达式`（或 `:=`），保留原生连接级变量，应用仍可通过同一连接的 `SELECT @变量` 读取；事务回滚不撤销变量赋值。变量名限定为 1–64 字节的裸 ASCII 标识符（首字符为字母或下划线，其余为字母、数字、下划线、`$`），不跟随任务的对象大小写转换。首版只接受有符号整数列、int64 范围常量、NULL、括号、一元正负号及加减乘，操作数明确转为 bigint；冻结实际引用列的源 `COLUMN_TYPE`。一元取负要求能证明操作数不为最小 bigint；无法证明时报告需要人工适配，避免两端对最小值取负的行为差异。最小 bigint 负字面量本身可用。不支持 unsigned、zerofill、decimal、浮点、RHS 读取变量、系统变量、多变量同时赋值及隐式类型转换。其他目标和兼容模式明确报告不支持。安装和修复重试在实际事务连接上再次检查目标能力。

目标函数采用 SECURITY INVOKER，不映射 MySQL DEFINER。时间函数使用语句起始时间和目标业务会话时区，两端业务连接应保持相同时区。触发器外的普通表默认值、ON UPDATE 等既有迁移语义不由本功能扩展。

未知函数、过程调用、循环、上述范围之外的变量用法、动态 SQL、复杂 DML、跨 database 引用、其他排序规则或文本大小排序、隐式类型转换、时间比较、执行顺序依赖，以及 BEFORE INSERT 读取自增列等，均报告为不支持自动转换。INSERT 遗漏动态默认列、UPDATE 遗漏 ON UPDATE 列也需人工适配；可以在源触发器中显式赋予受支持的表达式。同表/同事件/同时机多个触发器整组不安装。字符或数值元数据读取失败仅附加到实际缺失所需元数据的转换错误，不再附加到不支持语法等无关诊断。

一个逻辑触发器包含函数和触发器两个执行单元，整体事务安装，历史数据及行数验证完成后才安装。单个失败不停止其他对象。报告保留源定义与具体原因；原生成 SQL 可以整体只读重试，未知提交、结构/映射改变、数据依赖尚未完成时禁止重放。现有对象不会被覆盖或接管。

v3 及后续计划将配套函数命名为“按任务大小写设置处理后的源触发器名 + `_fn`”，例如 `update_enableuser_fn`；触发器本体仍沿用源名称映射。函数名超过 63 字节时，按完整 UTF8 字符保留最多 47 字节源名前缀，再接 `_`、12 位稳定身份摘要和 `_fn`。同批函数名冲突在准备阶段明确失败，目标已有不一致的同名函数时拒绝覆盖，不临时换名。

缺省版本及 v1、v2、v3 计划仍按原版本的名称、标记和 SQL 校验，不升级已有冻结 SQL，也不重命名已安装对象。v3 沿用 v2 转换语义及字符权重摘要规则。原报告中“不支持自动转换”且没有计划或执行记录的对象，可以在修复页“查看诊断 → 重新生成触发器方案”中预览两个只读执行单元，新方案使用 v4；核对保存后再确认执行。预览不写入修复记录、不执行目标 SQL；保存重新读取源定义并校验对象、连接绑定、依赖表版本和方案摘要。原源定义改变时拒绝替换，原报告保留。

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
| 商业 GaussDB、瀚高、海量、GBase、海云、人大金仓其他版本/兼容模式 | 对应实际版本与兼容模式 | 已接入共享实现，缺对应实库，尚未验证 |

openGauss-lite 5.0.3 的本次环境未提供可用 UUID 函数，现有 `get_uuid` 初始化失败；既有 `changedatatohistory`、`changedatatomain` 的能力探测也因 `ON COMMIT DROP` 不支持而失败。这些独立兼容项会影响整体任务状态，触发器行为测试保留并输出这些诊断，不将其报告为完整任务全部成功。

## v4 Kingbase 会话变量实测（2026-10-22）

`TestMySQLKingbaseNativeVariableIntegration` 在 MySQL `8.0.42 Source distribution` 与 KingbaseES `V009R003C018` MySQL 模式的随机 database/schema 中通过。覆盖 `modelmaxnumber_before`、`modelmaxnumber_after` 两条定义及 bigint 边界触发器的全量迁移、同连接变量读取、大小写、NULL、连接隔离、INSERT/UPDATE 回滚后变量保留、int32 上界加一与 signed bigint 溢出，以及原失败诊断重新生成方案和完整修复重试。测试清理全部夹具。

```sh
DBGOLD_MYSQLPG_TRIGGER_PRODUCT=kingbase /Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestMySQLKingbaseNativeVariableIntegration$' -count=1 -v
```

仍需设置上文两个 DSN 环境变量；凭据不写入代码或测试输出。

## v3 命名验证（2026-10-22）

在隔离 PostgreSQL 16.15 中，六类触发器均使用源名加 `_fn` 的配套函数，系统目录核对 `pg_trigger.tgfoid` 绑定与计划一致，原有 INSERT/UPDATE/DELETE 审计行为通过。同名不同定义函数冲突会拒绝安装，并确认原函数内容未被覆盖、未产生触发器。v0/v1/v2 冻结 SQL 的 JSON 恢复、整体安装及严格复用重试由目标层回归覆盖。其他 PG 内核的此次命名变更未单独实测。
