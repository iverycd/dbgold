# MySQL → SQL Server 2008 R2 验收

实现基线为 MySQL 5.7 / 8.0 和 SQL Server 2008 R2（10.50.x）。源数据在迁移期间应保持稳定；这不是 CDC。

## 功能边界

- 目标 Database 由连接指定，Schema 默认 `dbo`，须预先存在。
- 完整/仅结构迁移重建选中表；仅数据模式校验目标列类型、可空及自增属性后追加，不清空。已有外键保持启用，追加按源外键依赖排序，循环依赖明确失败。
- 表间可以并发；每表分页串行，批次内事务写入。失败批次回滚并停止该表，先前提交的数据可能保留。
- 参数化 INSERT 每句最多 1000 行、2000 参数。IDENTITY 会话使用专用连接，清理后连接不进入复用池。
- Unicode 文本采用 NVARCHAR，按 UTF-16 预留容量；长文本采用 NVARCHAR(MAX)。可空唯一键采用过滤索引。超过 900 字节键宽或 16 个键列明确失败。
- JSON、ENUM/SET、空间类型、生成列、ON UPDATE 规则、无法支持的默认表达式、DECIMAL 超过 38 位、前缀/函数/全文/空间/降序索引明确失败。不会自动转文本、丢弃规则或截断数据。
- TIME 仅接受 00:00:00 至 23:59:59.999999，负数/超过一天的 MySQL 时长、零日期、精度损失均失败。
- 视图使用 AST 转换，支持普通 SELECT/JOIN/聚合/子查询/CASE/UNION 和已实现函数。整数运算显式提升为 DECIMAL，超出 38 位或无法推断类型则失败。MySQL 非零 LIMIT 偏移、无 LIMIT 的 ORDER BY、窗口、GROUP_CONCAT 等未实现语法明确失败。UNION 要求显式列名及兼容的列类型；除法/AVG 读取 MySQL 的 div_precision_increment。视图不保证跨厂商排序规则语义一致。
- 函数和存储过程仍原样导出，触发器不转换。

## 自动化检查

在项目根目录运行：

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... ./api/handler/...
/Users/kay/sdk/go1.25.5/bin/go build ./...
```

在 frontend 目录运行 `npm test`、`npm run build`。

## 实库测试（待提供环境后执行）

准备 MySQL 专用测试数据库，以及 SQL Server 专用测试数据库中的已有 `DBGOLD_` 前缀 Schema。测试账号需能在这些测试位置创建/删除表、视图、索引、外键、扩展属性和执行 DBCC CHECKIDENT。

通过环境变量设置：

- `DBGOLD_MYSQL_SQLSERVER_TEST_DSN`：MySQL DSN，必须包含专用测试数据库。
- `DBGOLD_SQLSERVER_TEST_DSN`：SQL Server DSN，必须包含专用测试数据库，沿用可连接该实例的加密配置。
- `DBGOLD_SQLSERVER_TEST_SCHEMA`：已有的专用测试 Schema。
- `DBGOLD_SQLSERVER_TEST_ALLOW_DDL=1`：启用测试对象创建和清理。

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestMySQLSQLServer2008Integration$' -count=1 -v
```

该测试验证真实版本必须为 10.50.x；创建随机后缀测试表/视图，验证中文及补充字符、精确小数、日期精度、二进制、主键/唯一键/外键、注释、视图结果、自增续写和追加失败回滚，结束后仅清理这些测试对象。分别在 MySQL 5.7 和 8.0 上运行，保存版本和结果。

未设置环境变量时实库测试会跳过；本地测试/编译通过不代表已经通过 SQL Server 2008 R2 实库认证。不要用新版服务器的兼容级别测试替代它。
