# PG 系 → MySQL 全量迁移

## 使用方式

七类源库已开放：PostgreSQL、GaussDB、HighGo、Vastbase、GBase、SeaBox、KingBase。GaussDB 使用 opengauss 驱动，HighGo 保留专用 highgo 驱动，其余使用 postgres 驱动。实际产品、版本、兼容模式、目录字段和函数能力保存在报告中；未知目录或权限不足会报错。

连接配置中的 database 是建立连接的数据库。迁移页的源 schema 在该库内选择，未指定时为 `public` 并验证存在。一个源 schema 映射到一个已存在的 MySQL database。源对象名保持原始大小写，写目标前才应用用户的名称转换选项。

1. 迁移前停止源库写入及结构变更，直至复制和校验结束；本功能不包含 CDC。
2. 在数据、对象或视图迁移页运行兼容预检，查看问题、对象映射及 DDL。预检只读，不创建临时目标表。
3. 根据问题明确排除独立对象或整表，然后重新预检。列、未知类型属性和从属自增属性不能单独排除。
4. 启动后，后端重新执行同一预检；执行本次检查生成的元数据和 DDL，客户端不能提交已批准计划来绕过检查。
5. 在报告中查看能力信息、排除列表、阻止项、执行错误及最终行数对比。任何失败或行数不符都会使任务失败。

存在阻止项时，本次任务对目标执行零次 DDL/DML。进入执行阶段后，MySQL DDL 不具备整个任务级回滚能力；运行时网络、权限或资源变化造成的失败会报告并停止，已完成的页事务/DDL 不会自动恢复。目标在迁移期间也应保持不被其他任务修改。

## 已实现映射

普通整数、有限浮点、有界 `numeric(p,s)`、布尔、UUID、二进制、字符、日期时间和 JSON 使用统一元数据转换。NUMERIC 要求 `1 <= p <= 65`、`0 <= s <= 30`、`s <= p`；无界 NUMERIC、负 scale、数组、范围、interval、domain、enum 和未知扩展类型阻止整表。字符容量和 InnoDB 行宽/索引长度不足时阻止，不自动缩短索引或改成文本。

`timestamptz` 转 UTC `DATETIME(6)`；普通 timestamp 保留墙上时间。时间必须在目标范围内，且不丢失微秒精度。布尔写入 0/1；bytea 参数化写入二进制；JSON 检查有效性与数值范围，明确提示键顺序、重复键及表示规范化差异。

表和列注释随建表生成。对象模式修改列注释时读取 `SHOW CREATE TABLE` 完整列定义，保留默认值、自增及其他属性。每表最多一个非空整数自增列，建表时创建所需索引；保留导入的零值，完成后设置源序列下一值与导入最大值共同决定的种子。共享、独立、循环、非单位正步长、耗尽及无法保持上界语义的序列阻止自动转换。

常规 BTREE 索引和主键保留列顺序，升序索引的 NULL 位置采用 MySQL 默认行为；仅设置 NULLS FIRST（`indoption=2`）不会阻止，主键和唯一性约束仍保留；表达式、部分、INCLUDE、降序索引、未知索引选项、非默认操作符类、特殊访问方法、延迟唯一性及 NULLS NOT DISTINCT 均阻止，可明确排除独立索引。外键按目录数组位置配对，保留引用 schema，检查复合列、引用键、类型、动作和依赖；跨 schema、SET DEFAULT 和延迟外键不自动改写。

分区（包括可探测的 Gauss pg_partition/parttype）、继承、外部表、物化视图、生成列、排除约束和行级安全策略会显式阻止，避免重复或不完整复制。函数、过程、触发器不跨库转换；全量迁移中列出并要求明确排除，原始定义仍可单独导出。

## 目标版本与表达式

仅接受 MySQL 5.7 和 8.0，解析实际补丁版本，不按连接标签推断能力。使用 InnoDB、DYNAMIC，建表不指定字符集或排序规则，继承目标 database 默认配置（不是服务器默认配置）；读取严格模式、大小写策略、页大小、大索引能力和 max_allowed_packet。执行连接设置 UTC、NO_AUTO_VALUE_ON_ZERO、NO_BACKSLASH_ESCAPES 和严格模式，结束后恢复，不修改全局配置。

| 版本 | 默认值 | CHECK |
| --- | --- | --- |
| 5.7、8.0.0–8.0.12 | 已校验的常量、合法 CURRENT_TIMESTAMP | 阻止或明确排除 |
| 8.0.13–8.0.15 | 增加白名单表达式及大对象字面量默认值 | 阻止或明确排除 |
| 8.0.16 起 | 同上 | 受限简单表达式 |

默认值词法处理保留引号、空字符串、反斜杠和字符串内 `::`，不透传 PG 函数，不将错误表达式替换为 DEFAULT NULL。当前函数白名单为 NOW/CURRENT_TIMESTAMP 和受版本限制的 CURRENT_DATE。timestamptz 的当前时间默认值转换为 UTC_TIMESTAMP(6)，避免应用会话时区改变 UTC 存储值，因此需要 8.0.13+；5.7 遇到该默认值会阻止。精度截断的类型转换、未知函数和复杂表达式阻止。

视图支持列投影、别名、同 schema 表/视图、INNER/LEFT JOIN、基本比较、AND/OR/NOT、IS NULL、可安全转换的常量，并按依赖排序。CTE、子查询、聚合、窗口函数、复杂函数和 PG 特有运算符阻止。谓词中尚未实现的时间、UUID、JSON、二进制类型转换也阻止，避免仅删除 cast 改变语义；简单类型常量投影可通过目标类型校验。不会进入旧的按视图名称改写分支。

## 首版边界

- `data_only` 要求空的目标表，逐列检查名称、顺序、映射类型、NULL、自增、生成属性及排序规则，检查已有主键/唯一性、CHECK、外键和触发器；保留约束并先加载父表。非空追加、循环或自引用外键的数据导入暂不支持。
- 已有表的对象迁移要求 InnoDB DYNAMIC 和匹配列结构。外键引用表必须纳入完整验证，不能依赖未核验目标。
- 权限检查目前接受可确认的全局或直接 database 级授权；只通过角色继承、表级或不能确定作用范围的通配授权获得权限时失败关闭，需提供明确权限。
- 数据预检扫描全部数据，字符键通过目标 MySQL 的只读批量查询获取比较权重，唯一键验证使用长度编码的 SHA-256 键集合，内存随唯一键数量增加；外键会再次扫描子表。复制当前使用顺序表/分页，页大小最多 20000；并发参数不用于这条严格执行路径。
- 忽略源端列及索引的排序规则（包括显式指定的规则），不因排序规则阻止迁移；非默认操作符类和其他不支持的索引特性仍然阻止。已有表使用各列实际字符集和排序规则，不强制与数据库默认值一致。
- 按目标规则检查大小写、重音、CHAR 存储和 PAD SPACE／NO PAD 差异下的唯一性、外键及 CHECK；字符转换必须可无损回转，行宽和索引长度使用实际字符集字节数。迁移后应用比较/排序遵循目标配置。
- 执行前复核数据库默认配置和已有列排序规则；变化时停止并要求重新预检。预检不修改任何目标表，也不创建临时表。
- MySQL 5.7 重启后计数器可能从表内最大值重建，不能承诺 PG 序列空洞持久性。GENERATED ALWAYS 映射为 AUTO_INCREMENT 后允许显式赋值，报告会提示。
- 预检不证明厂商版本兼容认证，也不预测执行时发生的权限、磁盘或连接变化。没有实库测试的产品组合始终显示“待验证”。

## 接口及批量配置

`POST /api/migration/data/preflight` 使用现有启动请求字段，附加可选的 `excluded_objects`、`migrate_objects`、`table_names`、`view_names`。`migrate_objects` 与 `view_names` 互斥。连接访问权限在建立连接前验证。

```json
{
  "src_conn_id": 1,
  "dst_conn_id": 2,
  "src_database": "public",
  "target_schema": "destination",
  "migrate_mode": "all",
  "migrate_content": "both",
  "excluded_objects": [
    {"kind": "index", "table": "Orders", "name": "orders_partial"},
    {"kind": "table", "name": "UnsupportedTable"}
  ]
}
```

响应包含 `ready`、`source`、`target`、`issues`、`excluded`、`objects`、`rows_checked`；问题含对象身份、级别、原因、建议和是否可排除；每个计划对象含目标名称、阶段和 `statements[].SQL`。旧报告字段保留，新报告增加可选 `preflight`。排除列表保存到任务配置，所有启动入口重新验证。

批量 Excel 可添加 `source_schema` 列，未提供仍使用 public。批量页可按 Excel 行明确排除对象；multipart 字段 `excluded_objects_by_row` 为行号到对象数组的 JSON 映射，例如 `{"2":[{"kind":"view","name":"ComplexView"}]}`。Excel 校验只验证连接信息和迁移组合；每个任务实际启动前执行同一完整预检，失败会在该任务报告中展示。

## 验证矩阵和复现

2026-09-16 验证状态：

| 源 | 目标 | 状态 |
| --- | --- | --- |
| PostgreSQL 16.6（Homebrew） | MySQL 8.0.41（Homebrew） | 独立临时实例，全量迁移及字符规则集成测试通过 |
| PostgreSQL 16.6（Homebrew） | MySQL 5.7.20（Docker） | 独立临时容器，全量迁移及字符规则集成测试通过 |
| PostgreSQL | MySQL 5.7.44 | 待实库验证；提供固定镜像夹具 |
| PostgreSQL | MySQL 8.0.36 | 待实库验证；提供固定镜像夹具 |
| GaussDB、HighGo、Vastbase、GBase、SeaBox、KingBase | MySQL 5.7.44 / 8.0.36 | 待提供各产品具体版本、兼容模式及独立测试 DSN |

单元测试覆盖 8.0.12/13、8.0.15/16 门槛，不将模拟测试记为这些版本的实库通过。本次使用本机已有 MySQL 5.7.20 镜像验证，未运行 Compose 中的 5.7.44 / 8.0.36 矩阵。

在专用测试环境启动固定版本：

```sh
docker compose -f datamigrate/testdata/pgmysql/compose.yml up -d --wait
```

分别针对 55357 和 55380 执行，保存 `-v` 输出中的源产品、版本、模式和目标版本：

```sh
DBGOLD_PGMYSQL_SOURCE_DSN='host=127.0.0.1 port=55432 user=postgres password=dbgold_fixture dbname=fixture sslmode=disable' \
DBGOLD_PGMYSQL_TARGET_DSN='root:dbgold_fixture@tcp(127.0.0.1:55357)/fixture?parseTime=true&charset=utf8mb4' \
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestPGMySQL(Integration|TextComparisonIntegration)$' -v -count=1
```

国产源使用相同夹具，替换两个 DSN，并设置 `DBGOLD_PGMYSQL_PRODUCT` 和 `DBGOLD_PGMYSQL_DRIVER`（gaussdb/opengauss、highgo/highgo，其他产品/postgres）。夹具需要创建并删除独立 schema/database 的权限，只操作本次生成的 `dbgold_it_*` / `dbgold_text_*` 对象，结束时清理；未设置 DSN 时跳过实库测试。

夹具覆盖复合外键顺序、自增零值/序列空洞、identity、DECIMAL、二进制、中文与特殊名称、布尔、UUID、UTC 微秒、JSON、默认值、注释、视图、重复重建及结构/数据分阶段；检查阻止项不会删除已有目标数据。

```sh
/Users/kay/sdk/go1.25.5/bin/go build ./...
/Users/kay/sdk/go1.25.5/bin/go test ./...
npm test --prefix frontend
npm run build --prefix frontend
docker compose -f datamigrate/testdata/pgmysql/compose.yml down --volumes
```

实现依据：[PG 索引目录](https://www.postgresql.org/docs/current/catalog-pg-index.html)、[MySQL 默认值](https://dev.mysql.com/doc/refman/8.0/en/data-type-defaults.html)、[MySQL CHECK](https://dev.mysql.com/doc/refman/8.0/en/create-table-check-constraints.html)。

字符规则回归覆盖 `utf8mb4_general_ci`、`utf8mb4_bin`、`utf8mb4_unicode_ci`、`latin1_swedish_ci`，并在 8.0 覆盖 `utf8mb4_0900_ai_ci`。包括大小写、重音、Unicode 展开字符、尾空格、CHAR/VARCHAR、复合键、NULL、外键、CHECK（含不同列排序规则）、不可表示字符和已有列配置保留。源库测试使用显式列/索引排序规则，确保不再产生排序规则阻止项。空表及仅迁移结构时也检查字符默认值是否可无损转换。
