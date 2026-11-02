# 分区表迁移

通用迁移流程现在单独读取分区元数据，生成目标原生分区计划，并在数据写入前和完成后核对目标目录。分区表不会静默降级为普通表。该检查接入已有源库与目标库组合；能否执行由实际分区定义、约束、产品版本和可读取的目标能力共同决定，不能把“连接类型已接入”理解为所有分区形式均已验收。

## 使用方式与执行结果

- 迁移页不再提供手动分区预检入口。点击“开始迁移”并确认后自动检查分区兼容性，检查本身不会修改数据库；仅在发现问题时展示检查结果明细，包含受影响的逻辑根表、源分区方式、阻止原因、源目标叶分区名称映射和目标 SQL。全部表被阻止时，处理问题后再次点击“开始迁移”；局部阻止时跳过受影响表及其依赖对象，其余表继续迁移。完整分区记录请在迁移报告或历史任务详情中查看。所有新增列表每页固定 10 条。
- `ready` 表示全部所选表通过。`partition_preflight.can_start` 表示仍有可执行的表：局部阻止时只跳过受影响表及其依赖对象，其他表继续。PG 系 → MySQL 的其他兼容阻止项仍可阻止整个任务。客户端不能提交预检计划绕过后端复核。
- 只复制逻辑根表一次；物理子分区不再作为独立普通表重复复制。仅选择一个物理分区不会自动扩大为整张表，预检会要求选择根表。
- 支持完整迁移、仅结构、仅数据和原有修复入口。仅数据沿用追加行为：先冻结已有目标各叶行数，核验目标新增行数与源叶行数相等；不会自动清空目标。修复重导非空目标继续要求原有的明确清空确认。
- 仅数据迁移开启全量内容校验时，在写入前冻结目标根表及各叶的原值指纹；基线缺失、不可读或行数漂移时不向该表追加。迁移后逐项核对“原目标内容 + 源内容 = 最终目标内容”，保留重复行次数，也检查原目标内容是否被改变。临时文件只保存指纹，结束或失败后清理；报告目标行数表示本次新增量。
- 报告保存源结构、目标计划、名称映射、阻止和执行后核验结果；页面及报告均可导出分区记录。仅结构任务核对结构，不声称数据已校验。全量内容校验仍由原有开关控制。
- 分区或内容核验失败、取消、未完成会传递到补迁账本，不能仅因根表行数相等就记录数据成功。分区补迁开启内容校验时，写前冻结精确列映射，写后检查根表及每个叶的原值，再复核分区结构；缺少原值读取能力在清空目标前报错。

预检和迁移期间应停止源目标业务写入及结构变更。分区定义漂移、目录权限不足、部分 DDL 或数据失败、未知回执、分区边界/名称不一致或叶行数不一致都必须被报告，不能记为完整成功。任务级 DDL 和分批数据不构成一个整体事务，失败后可能保留已完成对象与已提交批次。

## 原生等价转换的边界

分区名称沿用现有对象大小写策略，不擅自截短或重命名；目标只支持原生编号时，报告保存源名称到目标序号的映射。目标使用默认表空间、文件组及其他物理存储设置，不复制源存储部署。

分区方法、键、常量边界、DEFAULT/NULL 归属、多列规则、子分区层级及 HASH 算法必须能由目标原生能力等价表达。只对经过显式规则验证的表达式进行转换，不直接执行源分区表达式，不增加业务列、触发器、辅助任务或定时预建分区。主键/唯一约束不能通过追加分区键、改成另一种唯一性范围或丢弃约束来勉强迁移。

更新分区键时是否允许行跨分区移动，同样属于必须保留的语义。Oracle 和达梦按原始 owner/table 读取 `ALL_TABLES.ROW_MOVEMENT`；缺失、不可读或未知状态会阻止该表，不根据产品默认值猜测。Oracle 的 `ENABLED` / `DISABLED` 定义见 [ALL_TABLES](https://docs.oracle.com/en/database/oracle/oracle-database/19/refrn/ALL_TABLES.html)；达梦原生开关及缺省禁用行为见[数据定义语句的 ROW MOVEMENT 子句](https://eco.dameng.com/document/dm/zh-cn/pm/definition-statement.html)。目标无法原生表达相同更新行为时，不能悄悄启用或禁用行移动。

MySQL 普通 HASH、LINEAR HASH 的单个有符号整数列及下述已证明整数表达式可以转换为 PG 声明式表达式 LIST 分区。转换保留原桶号、负数、NULL、整数极值及非二次幂分区数的路由，使用目标原生表达式，不添加业务列或后台任务。同库 LINEAR HASH 保留原生语法；无符号键和未证明表达式仍阻止。算法依据 [MySQL LINEAR HASH 官方说明](https://dev.mysql.com/doc/refman/8.0/en/partitioning-linear-hash.html)及 [MySQL 分区源码](https://github.com/mysql/mysql-server/blob/8.0/sql/sql_partition.cc)。

复杂整数表达式支持有符号整数列、整数常量、括号及嵌套 `+`、`-`、`*`、`DIV`、`MOD` / `%`、`ABS`、`FLOOR`、`CEIL` / `CEILING`。编译器按每列完整类型范围证明每一步中间结果不超出源有符号 BIGINT，并证明除数不可能为零；MySQL 对最小 BIGINT 负商的特殊 DIV 溢出也单独阻止。MySQL → PG 使用原生精确 numeric 运算，整数除法使用 `div`，保留负数向零截断与取模符号；MySQL → MySQL 保留原生运算。RANGE 的可空表达式仍检查 NULL 路由。无符号 SQL_MODE、潜在溢出/除零、浮点或未证明日期/时区/字符函数会报告具体原因，不以当前数据能通过作为全域证明。参见 [MySQL 分区限制](https://dev.mysql.com/doc/refman/8.0/en/partitioning-limitations.html)及[整数运算溢出规则](https://dev.mysql.com/doc/refman/8.0/en/out-of-range-and-overflow.html)。

PG 表达式目录核验还读取实际函数、运算符和非内置类型依赖；自定义域类型即使名为 `int8`、反解析后看起来像内置 bigint，也不能冒充原生类型转换。自定义或扩展实现即使名为 `abs`、`mod`、`div`、`coalesce`、`+` 或 `*`，也不能冒充内置路由函数。带引号名称、类型转换、运算树及特殊字符列名均参与指纹，不能通过文本相似绕过核验。

KEY / LINEAR KEY 支持官方 MySQL 5.7/8.0 InnoDB 同一正式版本（major.minor.patch）的原生迁移，显式保留 `ALGORITHM=1/2`。读取 `SHOW CREATE TABLE` 的版本注释、空 `KEY()` 使用的主键/非空唯一键、实际散列列顺序及完整类型/字符排序规则。原生散列依赖存储格式，因此 KEY 列按已验证语法恢复其原始类型和 collation；整数、DECIMAL、BIT、CHAR/VARCHAR、BINARY/VARBINARY 可用，未证明的时间旧格式、ENUM/SET 等阻止。未证明的 MySQL 版本差异会明确拒绝。跨产品仅放行下面的精确规则，绝不换成目标 HASH。

跨库 KEY / LINEAR KEY 新增官方 **MySQL 8.0.36 InnoDB → PostgreSQL** 的原生表达式 LIST 转换，支持 `ALGORITHM=1/2`、单列/多列整数键，总存储宽度最多 4 字节：有符号 TINYINT/SMALLINT/MEDIUMINT/INT，以及无符号 TINYINT/SMALLINT/MEDIUMINT。按物理字段顺序复现小端字节散列、算法 2 的 latin1 字节权重和尾部空格字节处理、NULL 更新规则、32 位截断及 LINEAR 非二次幂桶分配。使用 PostgreSQL 内置函数和原生 LIST，源值域以表达式中的拒绝分支保留；超域值在根表、直接叶写入和 UPDATE 中均无可用分区。无需辅助列、函数或后台任务。源码证据固定在 [mysql-8.0.36](https://github.com/mysql/mysql-server/tree/mysql-8.0.36)，不从版本相近推断相同实现。

跨库 KEY 的源列类型/可空性、目标原生整数存储类型/可空性以及完整路由表达式均纳入复核，覆盖根表与物理子层；仅数据和修复不能用改过类型、NULL 约束或值域守卫的已有表绕过检查。BIGINT、unsigned INT、超过 4 字节的组合、字符/小数/二进制等跨库键以及其他源版本仍明确阻止；目标表达式分区无法保留的主键/全局唯一约束、尚未完整迁移的源 CHECK、已有目标根表或叶分区上的额外 CHECK 也会阻止，不能删除约束来迁移。当前表达式展开上限 512 KiB，限制会直接展示在预检原因中。

INTERVAL 的通用路径新增 Oracle → DM8 的单列时间规则，保留原生自动扩展，目标必须实际确认 `MPP_INI=0`。支持无时区 DATE / TIMESTAMP(p<=6) 的整数月、整数秒规则，转换点必须为整秒，月规则的转换日不超过 28 日；TIMESTAMP 列本身可保留不超过 6 位的小数秒数据。同时冻结初始 RANGE 转换点、自动叶标志和真实上下界，保留稀疏区间。建表后使用达梦原生路由恢复现有自动叶（包括空叶）：在空表中写入显式安全常量并回滚临时行，核对目录后恢复叶名；后续新分区仍由目标原生 INTERVAL 自动生成。执行器拒绝生成/自增列、特殊默认值、已有触发器或业务约束等可能产生副作用的情况，失败步骤可凭完整冻结计划单独恢复。可空键、复合模板、时区或微秒规则，以及不具备已验证原生自动规则/恢复能力的目标仍阻止。仅数据迁移只核验已有拓扑，不执行空叶恢复。

数值 INTERVAL 的元数据与真实边界已补齐，但 Oracle → DM 数值规则明确阻止：实测 Oracle `NUMBER(5)` 配 `INTERVAL(10)` 可以写入 99999、生成上界 100000，达梦同精度键会在生成该上界时溢出；小数规则也存在对应差异。扩大目标精度并用原生 CHECK 或 SQL DOMAIN 限回源值域，也不能保留语义：达梦拒绝越界输入（包括舍入后越界的输入）后仍会留下额外自动空叶，事务回滚也不会撤销，而 Oracle 拒绝时不改变分区集合。DM8 实测还拒绝 `RANGE(CAST(K AS NUMBER(...)))` 与 `RANGE(K+0)`，不能仅扩大路由边界精度。[达梦数据定义文档](https://eco.dameng.com/document/dm/zh-cn/pm/definition-statement.html)的范围分区规则说明边界按分区列类型转换，SQL DOMAIN 列继承域的类型与 CHECK。目前没有已验证的无损原生替代方案，不扩大列精度、不添加辅助列/触发器或退化成静态 RANGE。达梦 → 达梦继续走已有独立原生流程。

INTERVAL 还按源键完整类型上界计算所有可能产生的叶数，连同初始 RANGE 必须不超过 65,535；不能只看当前已有几片分区。超出该范围会报告阻止原因，因此宽数值域、小时间步长或跨度很大的日期规则不保证可迁移。此门槛保留后续持续写入的容量边界，依据 [Oracle 分区编号上限](https://docs.oracle.com/en/error-help/db/ora-14300/)和[达梦原生分区总量与 MPP 限制](https://eco.dameng.com/document/dm/zh-cn/pm/manage-partition.html)。

SQL Server 单列 tinyint/smallint/int/bigint 的 RANGE LEFT 可用精确整数后继值改写为半开范围，保留原叶归属。有限边界为整数最大值、从而需要保留一个目标无法表达的空范围时仍阻止；不会溢出或删除该叶。浮点、时间、任意表达式等不套用整数加一规则。

当前无法证明等价的形式明确阻止，包括未知产品目录、上述整数规则之外的跨产品 KEY/LINEAR KEY、上述显式规则之外的跨产品 HASH 路由、依赖字符排序规则的未验证转换、NULL 路由不一致，以及上述范围之外的 INTERVAL 定义。已有自动分区的静态快照不能替代其未来生成规则。目标不支持的分区方法、外键或唯一性约束也不能通过丢弃对象、追加分区键或普通表回退绕过。

## 目标适配范围

下表表示已实现的原生构建及目录核验入口，不代表每个源目标组合和厂商版本已实库认证。实际预检仍检查键类型、边界、NULL、行移动、名称、约束和目录权限。

| 目标 | 已实现入口 | 额外限制 |
| --- | --- | --- |
| PostgreSQL 及可探测的兼容目录 | 声明式 RANGE/LIST/HASH、递归子分区；受控 MySQL HASH/LINEAR HASH 转换；PostgreSQL 本身另支持上述 KEY/LINEAR KEY 整数转换 | HASH/DEFAULT/行移动依赖实际版本；兼容库必须探测真实目录 |
| Gauss 原生 pg_partition 目录 | RANGE/LIST、多列 RANGE、LIST DEFAULT、子分区 | 原生表达式与自动扩展不凭产品别名放行 |
| MySQL | RANGE/LIST、COLUMNS、同库 HASH/LINEAR HASH、同正式版本 InnoDB KEY/LINEAR KEY、原生两级子分区 | 无原生 LIST DEFAULT；分区 InnoDB 的外键与唯一键限制仍生效 |
| Oracle / 达梦通用链路 | RANGE/LIST/HASH 元数据及原生两级声明、LIST DEFAULT、行移动子句；上述 Oracle → DM8 时间 INTERVAL | 跨库 HASH、复合或未证明日历规则仍阻止；Oracle 目标的自动空叶恢复尚未放开 |
| SQL Server | 单列连续 RANGE、原生分区函数及分区方案 | 叶名以原生序号映射；保留真实目标 Schema；LIST/HASH/子分区不冒充 RANGE |
| 神通 Oscar | 7.x 非 KSTORE 原生目录下 RANGE/LIST、多列 RANGE、LIST DEFAULT、两级子分区 | JDBC 原生目录及完整 DDL 必须可读；不能保留禁用行移动的源语义；尚未实库认证 |

神通独立适配使用厂商随附 2025-01-17 开发手册定义的 `V_SYS_PART_TABLES`、`V_SYS_TAB_PARTITIONS`、`V_SYS_TAB_SUBPARTITIONS`、`V_SYS_TABLE.SYS_GET_TABLEDEF` 和原生 OID 父链，不假设 PostgreSQL/Oracle 系统目录兼容。结构、计数和内容核验都使用原生物理叶身份。

达梦 → 达梦继续使用独立的原生整 Schema 流程，保留已有完整预检、原生对象恢复和故障接续语义，不改为通用的逐表部分执行。CDC 首次全量导入和增量路由不纳入本期分区保留能力，不能因共用 Migrator 而自动启用。

## 验证状态

单元测试覆盖目录读取、大小写、常量与边界、父子去重、版本/能力阻止、计划映射、局部失败、分区核验和修复快照；前端测试覆盖部分启动、全阻止、配置变更使预检失效、晚到响应、映射和行数展示。模拟测试及编译通过不等于产品实库认证。

现有实库夹具的连接变量和版本记录分别位于 `datamigrate/testdata/pgmysql`、`datamigrate/testdata/oracle`、`datamigrate/testdata/sqlserver`、`datamigrate/testdata/oracledameng` 及达梦原生文档。新增分区支持需要在实际源目标版本与兼容模式上运行专用夹具，记录目录/边界、逐叶数据、迁移后真实业务写入和未来自动扩展行为；未配置实库而跳过的测试仍属于未验收。现有普通表或触发器实库记录不能替代分区认证。

2026-10-30 在本次新建、无业务数据的独立临时实例中通过：

- MySQL 8.0.36 → PostgreSQL 16.15：RANGE、LIST、HASH、LINEAR HASH；逐叶原值及空叶、非空追加、真实更新后的分区归属。
- PostgreSQL 16.15 → MySQL 8.0.36：RANGE、LIST；逻辑根表去重、空叶、重复行、非空追加及根表/逐叶原值基线校验。
- MySQL 8.0.36 → SQL Server 2017 14.0.3550.4：RANGE；非默认 Schema、完整/仅结构/追加、原生序号映射、逐叶原值及后续跨分区更新。
- SQL Server 2017 14.0.3550.4 → PostgreSQL 16.15：单列 BIGINT RANGE LEFT，含最小/最大整数数据、有限边界邻点、空叶、逐叶原值及后续跨分区更新。
- MySQL 8.0.36 → PostgreSQL 16.15：嵌套整数表达式 RANGE/LIST/HASH/LINEAR HASH，含整数类型极值、负数除法/取模、NULL、重复行、后续 INSERT/UPDATE 和非空追加后的根表/逐叶内容校验。
- PostgreSQL 16.15：真实目录依赖核验，内置表达式允许，自定义同名函数、运算符及 `int8` 域类型遮蔽均被阻止；原生列域与修复另有 10 个真实场景，覆盖各层目录漂移、CHECK、单连接池及错误类型不能记为修复成功。
- MySQL 8.0.36 → PostgreSQL 16.15：`BIGINT DIV 1` 即使当前仅有安全值，也因未来最小整数溢出差异在预检阻止；完整/仅结构两模式均未创建目标关系。
- MySQL 8.0.36 → PostgreSQL 16.15：KEY/LINEAR KEY 的 9 类整数场景，包括算法 1/2、多列 NULL、TINYINT 全部值、整数边界/随机值、非二次幂桶和两级 RANGE + LINEAR KEY；完整迁移、逐叶原值、后续 INSERT/UPDATE 与非空追加通过。根表/直接叶超域写入拒绝；移除表达式值域守卫、改为 numeric 列、收紧根表/物理叶 NULL 约束、额外目标 CHECK，以及尚未迁移的源 CHECK 均在追加前阻止。
- MySQL 8.0.36 → MySQL 8.0.36：KEY/LINEAR KEY 的 8 种场景，包括算法 1/2、隐式 PK/UK、两级子分区及整数/字符/DECIMAL/BIT/二进制键；全字段逐叶内容、后续 INSERT/UPDATE、非空追加通过，目标 collation 不一致在写入前被阻止。
- Oracle 11.2.0.4 → DM8 20240920：DATE 按月（转换点 5000 年）、DATE 按天及 TIMESTAMP(6) 按天（转换点 9900 年），共 6 个完整/仅结构场景，满足完整日期域的分区容量证明；空自动叶、稀疏空洞、删除近端空叶后的单独恢复、根表/逐叶原值、未来自动扩展和最大日期边界均通过。数值整型/小数极值、可空键、超容量域、微秒规则作为不等价或未证明场景阻止。新增整数/小数各完整与仅结构负向回归：验证同精度上界溢出，以及扩大精度后 CHECK/DOMAIN 拒绝直接越界和舍入越界输入仍留下自动空叶；CAST 分区表达式也实测语法拒绝。

神通实库验收按用户本轮要求暂缓，仍属于未验收。神通夹具已开启非空追加内容校验；本地模拟覆盖根表/叶分区/空叶、重复行、原内容篡改、NUL、尾空格和资源清理，这些结果不替代实库认证。

专用夹具用 `DBGOLD_PARTITION_ALLOW_DDL=1` 和对应 `DBGOLD_PARTITION_MYSQL_DSN`、`DBGOLD_PARTITION_PG_DSN`、`DBGOLD_PARTITION_SQLSERVER_DSN` 等变量显式启用，只创建唯一命名的测试对象并清理。上述结果不扩展为其他厂商或版本的通过记录。
