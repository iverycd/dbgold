# MySQL → PG 内核数据库空间类型迁移

迁移保留几何对象、坐标精度、逐值 SRID、列级 SRID 限制和空间索引。无需新增前端选项；选择现有 MySQL 源连接和 PG 系目标连接即可使用。全量迁移、仅数据迁移、CDC、索引重试和数据补迁使用同一套空间能力检查。

## 支持范围

| MySQL 类型 | 目标空间类型 |
|---|---|
| `GEOMETRY` | 空间扩展的 `geometry` |
| `POINT` | `geometry(Point)` |
| `LINESTRING` | `geometry(LineString)` |
| `POLYGON` | `geometry(Polygon)` |
| `MULTIPOINT` | `geometry(MultiPoint)` |
| `MULTILINESTRING` | `geometry(MultiLineString)` |
| `MULTIPOLYGON` | `geometry(MultiPolygon)` |
| `GEOMETRYCOLLECTION` / `GEOMCOLLECTION` | `geometry(GeometryCollection)` |

- 使用空间扩展中的类型，类型、函数和 GiST 操作符类均限定实际 schema；支持扩展放在默认 `search_path` 之外的 schema。
- MySQL `SRS_ID` 为 NULL 时，不给目标列增加固定 SRID 限制；明确指定 SRID 时保留限制。SRID 0 额外使用 CHECK，避免将其解释为“未限制”。旧版 MySQL 缺少 `SRS_ID` 字段时，按无列级限制处理，仍保留每个值自身的 SRID。
- 原始 MySQL 空间数据转换为带 SRID 的 HEXEWKB；坐标二进制精度和顺序保持不变，不执行坐标变换。支持 NULL、混合 SRID、集合和空集合。
- 源空间索引的 `SPATIAL` / `RTREE` 元数据转换为单列非唯一 GiST 索引。MySQL 报告的 32 字节 MBR 长度不作为字符串前缀处理。
- CDC 识别具体的 `POINT`、`POLYGON` 等子类型；无主键整行定位使用统一字节序 EWKB 精确比较，区分相同包围盒中的不同几何对象。

PostGIS 接受 HEXEWKB 并保留 SRID，详见 [PostGIS 空间格式说明](https://postgis.net/docs/using_postgis_dbmanagement.html#EWKB_EWKT)。MySQL 的列级 SRID 元数据定义见 [INFORMATION_SCHEMA.COLUMNS](https://dev.mysql.com/doc/refman/8.4/en/information-schema-columns-table.html)。

## 目标环境与自动启用

只有所选对象包含空间字段或空间索引时才准备空间能力。优先复用已存在且经过实际读写验证的兼容 `geometry` 类型。缺少类型时，检查服务器可用扩展目录；存在 `postgis` 扩展包才尝试：

```sql
CREATE EXTENSION IF NOT EXISTS postgis;
```

启用之后仍会检查 HEXEWKB 输入、坐标和 SRID 往返、必要函数及 GiST 操作符类。扩展使用服务器默认安装位置，已启用的扩展保持原位置和版本。

自动准备不下载或安装服务器软件包，不修改实例参数，不重启数据库，不升级已有扩展，也不自动安装 raster、topology 等其他扩展。扩展包缺失、权限不足或产品参数不满足时，报告中保留具体错误，交由目标环境管理员准备。

基础空间能力缺失时，相关表在建表前失败，不执行该表的 DROP 或数据复制；其他独立表继续。仅 GiST 不可用时，空间索引记录失败，支持的建表和数据迁移继续。连接中断、任务取消等仍按任务级错误处理。

## 兼容性检查与错误处理

- 仅数据迁移、CDC 启动和补迁检查目标真实类型、子类型及 SRID 限制，拒绝将源数据写入普通文本、PG 自带 `point` 或会隐式更改 SRID 的目标列。
- 空间默认表达式和生成列表达式暂不自动翻译；建表前明确记录表列错误。普通 NULL 默认值可保留。
- 非法或截断的 WKB、类型与数据不一致、超过当前适配范围 `0..999999` 的 SRID 返回错误，不丢弃 SRID、不替换为 NULL，也不降级为文本或 bytea。
- 全量复制错误沿用表级失败报告；CDC 转换错误回滚当前事务，不推进 checkpoint。
- CDC 在事务表锁后重新核验本事务涉及的空间列；运行中目标 `geometry` 被改成 `text` 等不兼容类型时，回滚并保持 checkpoint，避免将空间数据静默写入普通字段。
- GiST 同名对象和重试会核验所属表、列、索引方法和操作符类，同名 BTREE 或错误定义不能被当作迁移成功。
- 补迁预览只读，不启用扩展；实际执行会重新核验空间能力和目标列。

自定义坐标系定义迁移、坐标系转换、业务空间函数及视图语法转换不在此次实现范围。数据中的 SRID 数值保留，自定义 SRS 定义需由目标环境单独准备。

## 验证记录

| 目标标识 | 验证状态 |
|---|---|
| `postgres` | 已实测 MySQL **8.4.11** → PostgreSQL **16.9** / PostGIS **3.5.2** |
| `gaussdb`、`highgo`、`kingbase`、`vastbase`、`gbase`、`seabox` | 已接入共享能力检测、映射和转换路径；尚未完成各产品版本的实库认证 |

PostgreSQL 实测覆盖：自动启用 PostGIS、复用已启用扩展、扩展位于不在 `search_path` 中且名称含空格的 `Geo Space` schema、八类空间类型、SRID 0/4326/混合 SRID、NULL、空集合、嵌套集合和非对称经纬度。目标 WKB 与源内部 WKB 逐字节比较，并单独比较 SRID。

同时覆盖 GiST 创建与核验、同名 BTREE 拒绝复用、索引修复和仅索引重试、补迁预览与清空后重载、仅数据迁移中不兼容空间表的隔离，以及空间生成列失败后后续普通表继续迁移。

### 运行测试

常规回归和构建：

```bash
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... ./api/handler
/Users/kay/sdk/go1.25.5/bin/go build ./...
```

实库测试必须使用独立、可清理的测试数据库。源库要求支持测试中的 MySQL 8 空间 SRID 声明；目标服务器须已有 PostGIS 扩展软件包。测试创建唯一源表和目标 schema，结束后清理这些对象，不删除共享扩展：

```bash
export DBGOLD_MYSQLPG_SPATIAL_SOURCE_DSN='testuser:testpass@tcp(127.0.0.1:3306)/spatial_source?parseTime=true'
export DBGOLD_MYSQLPG_SPATIAL_TARGET_DSN='host=127.0.0.1 port=5432 user=testuser password=testpass dbname=spatial_target sslmode=disable'
export DBGOLD_MYSQLPG_SPATIAL_PRODUCT='postgres'

/Users/kay/sdk/go1.25.5/bin/go test -tags integration ./datamigrate \
  -run '^TestMySQLPGSpatialIntegration$' -count=1 -v

/Users/kay/sdk/go1.25.5/bin/go test -tags integration ./datamigrate/cdc \
  -run '^TestPostgresSpatialCDCIntegration$' -count=1 -v
```

首次自动启用场景使用尚未执行 `CREATE EXTENSION postgis` 的测试数据库。非默认 schema 场景使用另一个独立数据库，预先把 PostGIS 安装到专用 schema，并让连接的 `search_path` 不包含该 schema，再运行同一测试。不要移动共享数据库中的现有扩展。

测试日志输出实际源库、目标库和扩展版本；仅当相应产品和版本通过实库测试后，才增加该组合的认证记录。
