# PG 内核 → Oracle 迁移

已开放 PostgreSQL、GaussDB、瀚高、Vastbase、GBase、Seabox、Kingbase 七类源连接到 Oracle 的迁移入口，以 Oracle 11g 的 SQL 和 30 字节名称限制为兼容基线。接入与能力探测通过不等于某个产品、版本、兼容模式已获实库认证；当前认证状态见文末矩阵。

## 使用方式

1. 配置源连接及源 Schema，选择 Oracle 目标连接及已有目标 Schema。Oracle 目标对象名统一转大写；源目录、复制读取及报告保留原始大小写。大小写折叠后的重名、超长名称会阻塞相关对象，不自动截断或改名。
2. 在数据迁移、仅对象迁移或视图迁移页运行预检。`ready` 表示所选范围无阻塞；`can_start` 表示仍有可执行的健康对象。部分对象被阻塞时，可检查原因或明确排除后继续；报告保留失败及依赖失败明细。
3. 预检只读取结构，不扫描全部业务数据。执行时重新建立计划并逐页、逐值校验。迁移期间保持源库及目标迁移范围停写，迁移完成后再切换应用。
4. 全量迁移可选结构与数据、仅结构或仅数据。仅对象迁移按能力显示 CHECK 约束、主键、索引、序列、外键和注释。视图单独迁移会核对目标依赖对象。
5. 查看报告中的结构预检、行数/内容校验、失败 DDL 和修复账本。数据页失败时，之前已提交的页保留，失败页不计作成功，后续独立表继续。报告的“自动编号触发器”指工具生成的序列触发器。

普通单表 DDL、数据或对象错误不会提前终止独立对象；失败表不会继续复制数据，依赖它的对象跳过。若 DROP 失败保留了原表，还会检查目标引用关系，保护该表所引用的后续表。连接失效、取消、关键账本持久化失败等任务级错误仍终止执行。

## 首期转换范围

| 对象或类型 | 自动处理范围与限制 |
| --- | --- |
| 整数、布尔 | `int2/int4/int8` → `NUMBER`；布尔 → `NUMBER(1,0)`，保留取值检查。 |
| 数值 | `numeric` → `NUMBER`，声明精度/标度须落在目标范围；`float4/float8` → `BINARY_FLOAT/BINARY_DOUBLE`。执行时检查实际值的精度、范围与特殊值。 |
| 字符、二进制 | 有界 `varchar/bpchar` 在实际目标字符集安全容量内映射 `VARCHAR2/CHAR`；`text`、无界 `varchar` → `CLOB`；`bytea` → `BLOB`。国家字符选项使用对应国家字符类型并保留字符长度限制；不能证明填充/长度等价的组合拒绝。 |
| 文本化类型 | `json/jsonb/xml` → `CLOB`；`uuid/inet/cidr/macaddr/macaddr8/bit/varbit` 使用受约束的字符表示。这些是表示转换，不提供 PG 原生类型的操作符、排序与索引能力。 |
| 日期时间 | `date`、精度 0–6 的 `timestamp/timestamptz`，目标分别为 `DATE/TIMESTAMP/TIMESTAMP WITH TIME ZONE`；实际值仍须通过范围检查。 |
| 默认值 | 受限的已识别常量、布尔、时间默认值及目录中有绑定证据的序列默认值；不执行任意源表达式。无法证明转换安全的默认值会明确报错。 |
| 主键、UNIQUE、普通索引 | 普通 BTREE 列索引及受支持主键/唯一约束；主键和唯一约束保留可延期及初始延期属性。拒绝 GIN/GiST/BRIN、表达式、部分索引、INCLUDE、非默认操作符类/排序规则、无效索引和 `NULLS NOT DISTINCT`。含可空列的复合 UNIQUE 因 NULL 语义差异被阻塞，依赖它的外键同步阻塞。 |
| CHECK | 当前表列上的受限比较、`AND/OR/NOT`、`IS NULL/TRUE/FALSE`及可证明类型安全的表达式；保留 SQL 三值逻辑和验证状态。排除约束、`NO INHERIT`、LOB 比较或未识别语法拒绝。 |
| 外键 | 已映射 Schema 内、引用受支持主键/唯一约束的 `MATCH SIMPLE`；保留可延期属性。仅允许 Oracle 可表示的更新/删除动作，延期 `RESTRICT` 不改写成可延期 `NO ACTION`。 |
| 序列及自动编号 | 原始序列定义、增减步长、状态、完整列绑定形成冻结计划；生成 Oracle 序列与工具专用触发器，并按已迁数据校准。Oracle 11g 实现不声称保留 PG `IDENTITY ALWAYS` 的全部写入限制；报告提示应用语义差异。 |
| 普通视图 | 同 Schema 已知对象上的显式列 `SELECT`、INNER/LEFT/RIGHT/FULL/CROSS JOIN、WHERE、GROUP BY、HAVING、DISTINCT、UNION/UNION ALL；COUNT/SUM/MIN/MAX/AVG、CASE、COALESCE、NULLIF 及少量安全类型转换。布尔投影转换为数值，谓词与数值分开处理。 |
| 视图限制 | 不自动处理 CTE、递归、窗口、子查询、通配符、ORDER BY/LIMIT、任意函数、未消费语法或跨 Schema 未映射引用。LOB 可直接投影，但不参与排序比较、分组、聚合、DISTINCT 或集合操作；空字符串相关比较不能证明等价时拒绝。 |
| 分区 | 复用原生分区计划和目标等价性核验，保存完整边界及叶映射。只有目标能力、键域、NULL 路由、上下边界和拓扑均通过检查的计划可执行；不把 HASH 算法、缺口、DEFAULT 或可空 RANGE 的差异静默抹平，也不自动降级为普通表。参见 [分区迁移说明](partition-migration.md)。 |
| 注释 | 表注释、列注释保留源身份并映射到目标名称。 |

数组、enum/domain、复合或扩展类型、未列入白名单的时间/区间类型、生成列表达式、复杂视图及特殊索引会留下明确诊断，不原样送往 Oracle 执行。源业务触发器、函数和存储过程不做自动跨方言翻译，按原始 DDL 导出后人工适配。

Oracle 空字符串视为 NULL。类型检查、默认值及视图转换会对相关差异报错或提示；不能将这条迁移路径当作完全等价的 PostgreSQL 执行环境。应重点核对空串与 NOT NULL、大小写名称、文本化类型、时间默认值和自动编号的应用行为。

## 先迁结构、后迁数据与补迁

仅数据迁移会核对目标列定义和触发器。允许执行的工具自动编号触发器必须有服务器持久化的成功序列账本作为依据：同用户、同连接与冻结端点、源数据库/Schema、目标 Schema，且冻结程序、目标 ObjectID 和定义指纹一致。用户提交的身份或目标同名对象本身不能作为归属证明。

历史有效证据中的同名身份冲突交给对象预检处理，相关序列和表被阻塞，独立对象继续。目标库再次核对实际序列身份、定义及绑定；源定义或完整绑定漂移也会阻塞。补迁使用相同冻结信息，并在相关对象完成后校准序列。已有手工业务触发器不会被自动认领。

## 自动化验证

无需数据库的回归检查：

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/pgoracle ./datamigrate -run 'TestOracle|TestPGOracle' -count=1
/Users/kay/sdk/go1.25.5/bin/go test ./api/handler -run 'TestPGOracle|TestCheckConstraint|TestOracleTargetRegistration' -count=1
cd frontend
npm test
npm run build
```

真实端到端夹具为 `TestPGOracleIntegration`。必须显式设置 `DBGOLD_PG_ORACLE_ALLOW_DDL=1`，并提供至少一个源 DSN 和一个目标版本的 DSN/Schema。目标 Schema 必须是专用测试 Schema，名称以 `DBGOLD_` 开头；测试不会创建 Oracle 用户。源账户需能创建和删除测试 Schema；每个单元新建随机的 `DBGOLD_...` 源 Schema，不接管已有 Schema。目标仅使用随机名称，并在执行前确认不存在同名对象；清理只针对本次确认的新对象。

源 DSN 变量为 `DBGOLD_PG_ORACLE_POSTGRES_DSN`、`DBGOLD_PG_ORACLE_GAUSSDB_DSN`、`DBGOLD_PG_ORACLE_HIGHGO_DSN`、`DBGOLD_PG_ORACLE_VASTBASE_DSN`、`DBGOLD_PG_ORACLE_GBASE_DSN`、`DBGOLD_PG_ORACLE_SEABOX_DSN`、`DBGOLD_PG_ORACLE_KINGBASE_DSN`。目标变量为 `DBGOLD_PG_ORACLE_11G_DSN` / `DBGOLD_PG_ORACLE_11G_SCHEMA` 与 `DBGOLD_PG_ORACLE_19C_DSN` / `DBGOLD_PG_ORACLE_19C_SCHEMA`。缺少的矩阵单元明确 SKIP，不尝试其他连接，不认定为认证通过。

```bash
# 通过本地安全环境注入 DSN；不要把含密码的 DSN 写入仓库或测试日志。
export DBGOLD_PG_ORACLE_ALLOW_DDL=1
export DBGOLD_PG_ORACLE_19C_SCHEMA=DBGOLD_PG_TEST
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestPGOracleIntegration$' -count=1 -v
# 也可只执行一个已配置单元：
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestPGOracleIntegration/postgres/19C$' -count=1 -v
```

夹具真实执行 Reader 快照 → 只读预检 → Run → 目标行数与普通视图结果核对 → 省略主键的后续 INSERT，验证序列继续分配且工具身份进入修复报告。输出实际源产品、版本、兼容模式、目录，以及 Oracle 版本与字符集。该夹具是首期主链验收，不覆盖全部分区、特殊字符集与异常恢复组合。

## 实库验证矩阵

本次未启用需要专用环境和显式 DDL 开关的实库夹具。下表全部为“未完成实库验证”；单元测试、SQL mock、能力探测及缺环境 SKIP 不改变此状态。后续应附上对应版本、兼容模式和夹具日志，再更新具体单元。

| 源产品 | Oracle 11g | Oracle 19c |
| --- | --- | --- |
| PostgreSQL | 未完成实库验证 | 未完成实库验证 |
| GaussDB | 未完成实库验证 | 未完成实库验证 |
| 瀚高 HighGo | 未完成实库验证 | 未完成实库验证 |
| Vastbase | 未完成实库验证 | 未完成实库验证 |
| GBase | 未完成实库验证 | 未完成实库验证 |
| Seabox | 未完成实库验证 | 未完成实库验证 |
| Kingbase | 未完成实库验证 | 未完成实库验证 |
