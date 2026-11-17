# 达梦 / SQL Server 跨库迁移

新增 `dameng → oracle`、`dameng → sqlserver`、`sqlserver → oracle`。使用现有连接、迁移、批量导入、工单、预检和修复入口，无新增必填请求参数。

## 范围

设计基线为 DM8、Oracle 11g+ 和 SQL Server 2012+。SQL Server 源严格限定连接所选数据库的 `dbo`；达梦 owner 保留配置中的原始大小写。Reader 不改写对象名，Oracle 写入时统一大写，SQL Server 使用现有大小写选项。

支持结构与数据、仅结构、仅数据追加、仅对象和仅视图。表、主键、普通/唯一索引、外键、注释和生成器分别执行；单表 DDL 生成、DROP、CREATE 或数据页失败不会取消独立表。失败表不继续复制，依赖对象记录跳过原因。连接故障、取消和关键账本持久化失败仍终止任务。

预检字段为 `kind: "cross"`，区分 `ready` 和 `can_start`。结构问题允许保留健康对象执行；排除对象随请求提交。预检检查元数据、类型、默认值、目标状态及对象计划，不宣称已扫描并验证每一行。执行重新读取并核验，报告保存可选 `cross_preflight`，旧报告继续可读。分区沿用能力检查与编译流程，不能等价转换的表不会降级为普通表。

## 类型、默认值与数据

- 数值使用带错误返回的类型映射。精确数值以十进制文本读取，避免经过 `float64`。Oracle NUMBER 校验有效数字、指数、精度和小数位；达梦到 SQL Server 的无界数值及十进制存储 FLOAT 使用已有精度分析。
- 达梦 FLOAT 读取实际存储描述，区分十进制、单精度和双精度。未知类型、计算列和无法判断的实际类型阻塞表。
- SQL Server GUID → `VARCHAR2(36 CHAR)`，XML → `CLOB`，rowversion/timestamp → `RAW(8)`，纯 TIME → `VARCHAR2(18 CHAR)`。预检提示表示变化；rowversion 不模拟自动更新。
- Oracle 时间戳保留 0–9 位小数秒；SQL Server 源 `datetime2` / `datetimeoffset` 的七位精度进入转换和数组绑定。达梦超过七位的小数秒不舍入到 SQL Server，而是阻塞表。
- Oracle 可空字符列空串转 NULL 并记录警告；非空字符列空串使当前页失败。原生 CLOB/BLOB 的空值与 NULL 使用不同绑定；不清除文本中的 NUL。RAW 无法区分空二进制和 NULL，遇到空二进制明确失败，需人工改为 BLOB。
- 字符集、字符容量、精度和范围在写入前检查。固定游标按页读取，每页使用目标事务；失败页回滚，已提交页保留，不丢弃失败行。仅数据追加按目标行数增量校验。
- 默认值只接受已实现的常量、NULL、当前时间表达式和直接序列绑定。表达式依赖多个序列、序列参与计算或未知函数不猜测转换，阻塞相关表。当前时间表达式遵循目标会话时间语义，应核对应用时区。

## 对象和恢复

Oracle 自动编号使用工具序列和 BEFORE INSERT 触发器。种子、步长、边界、缓存、原始绑定和工具对象 ID 写入冻结计划。独立及共享序列保留完整绑定范围；存在未选择或跨 Schema 的消费者时，相关生成器需要人工处理。循环、有序、超出目标数值范围或不明确的状态不自动续号。

复制后按步长方向检查所有消费者的 MAX/MIN，保持递增或递减数列。Oracle 既有序列必须有匹配账本身份，且目标对象 ID、定义和绑定触发器仍匹配，才能复用；不根据名称认领或 DROP 既有序列。序列创建成功后先保存对象 ID，再安装绑定。仅数据和补迁只校准已验证的生成器。

Oracle 11g 的工具触发器在显式 NULL 时也发号，与部分源默认值的行为不同。业务触发器不自动转换；函数和存储过程仍原样导出。

普通列索引和外键经过目标能力检查。特殊、过滤、包含列、表达式或失效索引保留失败原因。不能等价保持的唯一键 NULL 语义、外键动作和跨 Schema 引用单独报告。没有隐式增加分区键或丢弃约束。

Oracle 视图使用受限 SELECT 解析，支持显式列、JOIN、WHERE、GROUP BY/HAVING、CASE、COALESCE、NULLIF、UNION 和已实现的聚合/标量函数。达梦到 SQL Server 使用现有受限 Oracle 风格 SELECT 转换器。按依赖创建；跨 Schema、窗口、递归、未知函数或不支持语法保留源定义供人工处理。工具生成器不可在通用 SQL 编辑器中修改，可通过完整冻结计划重试；表数据补迁重新检查结构、行数、映射、生成器状态和绑定。

## 自动化检查

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./...
/Users/kay/sdk/go1.25.5/bin/go build ./...
cd frontend
npm test
npm run build
```

新增测试位于 `datamigrate/cross_*test.go`、`datamigrate/source/cross_reader_test.go`、`datamigrate/typemap/cross_oracle_test.go`、`datamigrate/dialect/cross_oracle_test.go`、`datamigrate/valueconv/cross_oracle_test.go`、`datamigrate/target/cross_oracle_test.go` 和 `frontend/tests/pgmysql-preflight.test.mjs`。覆盖原始大小写、dbo 范围、精确自增状态、直接/共享绑定、七位时间绑定、38 位数值、空串/空 LOB/NULL、分页回滚、失败表隔离、对象/视图模式、追加行数和三页签的预检请求。

## 实库验收：尚未完成

本次未配置专用测试库，三组合实库用例均为 **SKIP**；单元测试及构建通过不等于数据库版本认证。下列矩阵需要在交付环境逐项执行并记录结果：

| 源环境 | 目标环境 | 状态 |
| --- | --- | --- |
| DM8 原生兼容模式 | Oracle 11g / 19c | SKIP，未配置测试库 |
| DM8 Oracle 兼容模式 | Oracle 11g / 19c | SKIP，未配置测试库 |
| DM8 SQL Server 兼容模式 | Oracle 11g / 19c | SKIP，未配置测试库 |
| 上述 DM8 模式 | SQL Server 2012 / 较新版本 | SKIP，未配置测试库 |
| SQL Server 2012 / 较新版本，dbo | Oracle 11g / 19c | SKIP，未配置测试库 |

`TestCrossMigrationIntegration` 为三个组合共用的专用夹具入口。它创建随机命名测试表，验证仅结构后追加数据、精确小数、Unicode、七位时间和非 1 自增步长续写，结束后清理自身对象。不修改源兼容模式；每种模式使用单独配置的测试实例重跑。

只有显式设置以下变量才会运行 DDL。不要配置生产库：

```sh
export DBGOLD_CROSS_TEST_ALLOW_DDL=1
export DBGOLD_CROSS_TEST_DAMENG_DSN='专用 DM8 测试连接'
export DBGOLD_CROSS_TEST_DAMENG_SCHEMA='DBGOLD_DM_TEST'
export DBGOLD_CROSS_TEST_ORACLE_DSN='专用 Oracle 测试连接'
export DBGOLD_CROSS_TEST_ORACLE_SCHEMA='DBGOLD_ORACLE_TEST'
export DBGOLD_CROSS_TEST_SQLSERVER_DSN='专用 SQL Server 测试连接，database 以 DBGOLD_ 开头'
export DBGOLD_CROSS_TEST_SQLSERVER_SCHEMA='DBGOLD_SQLSERVER_TEST'
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run TestCrossMigrationIntegration -v -count=1
```

Schema 由测试管理员预先创建并授予测试账户所需权限。SQL Server 源数据库名必须以 `DBGOLD_` 开头，源 Schema 固定 `dbo`；其目标 Schema 单独配置且必须以 `DBGOLD_` 开头。DM 与 Oracle 配置的测试 Schema 也必须以 `DBGOLD_` 开头。各数据库应使用满足 Unicode 验收的字符集。测试不打印 DSN。

实库扩展验收还应包含独立/共享/降序序列、跨 Schema 同名表、特殊类型、目标冲突、人工制造的 DROP/CREATE 失败、补迁和源/目标外部并发写入的禁止条件。迁移和补迁期间停止相关源表及目标表的业务写入。

类型依据：[Microsoft rowversion](https://learn.microsoft.com/en-us/sql/t-sql/data-types/rowversion-transact-sql?view=sql-server-ver17)、[Microsoft 日期时间类型](https://learn.microsoft.com/en-us/sql/t-sql/functions/date-and-time-data-types-and-functions-transact-sql?view=sql-server-ver15)、[Oracle 11g 数据类型](https://docs.oracle.com/cd/E11882_01/server.112/e41084/sql_elements001.htm)、[达梦 SQL 附录](https://eco.dameng.com/document/dm/zh-cn/pm/sql-appendix.html)。
