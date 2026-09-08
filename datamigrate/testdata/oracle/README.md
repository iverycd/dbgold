# MySQL → Oracle 联调

兼容基线为 Oracle 11g；分别在 MySQL 5.7/8.0 → Oracle 11g/19c 上运行以下测试。未配置真实环境时测试会跳过，不能据此认定数据库兼容性已验证。

使用专用的 MySQL database 和 Oracle 用户/schema（名称以 `DBGOLD_` 开头）。Oracle 连接中的服务名与目标 schema 是两个独立字段。测试不创建用户、不授予权限、不修改服务器全局参数；只创建并清理带随机名称的测试表、视图、序列和触发器。

Oracle 测试用户需要 CREATE SESSION、CREATE TABLE、CREATE VIEW、CREATE SEQUENCE、CREATE TRIGGER，以及表空间配额。源库测试用户需要建表、建视图、读写、查询视图定义及删除测试对象的权限。建议两端字符集使用完整 Unicode。

在本地私有环境中设置（不要提交带密码的 DSN）：

- `DBGOLD_MYSQL_ORACLE_TEST_DSN`：MySQL DSN，必须包含专用 database。
- `DBGOLD_ORACLE_TEST_DSN`：Oracle DSN，使用 Service Name。
- `DBGOLD_ORACLE_TEST_SCHEMA`：上述专用 Oracle 用户的准确名称。
- `DBGOLD_ORACLE_TEST_ALLOW_DDL=1`：显式启用测试对象的创建和清理。

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestMySQLOracleIntegration$' -count=1 -v
```

测试覆盖大小写、跨 int64 自增、BIT(64)、38 位小数、微秒、长 Unicode CLOB、空 LOB 与 NULL、零日期、DOUBLE 大指数、外键、视图依赖、自增续号和重复全量迁移。任一必要能力失败均令测试失败，不自动缩减覆盖范围。

生产迁移建议在源库停止写入或使用静态副本期间执行；本功能不提供 CDC 或全库一致性快照。全量/仅结构沿用现有重建目标表行为；仅数据追加到已有表，失败只回滚当前分页，之前已提交的分页保留。

支持普通 SELECT/JOIN/过滤/分组/COUNT、SUM、AVG、MIN、MAX/UNION 和部分常用函数。LIMIT、窗口、自定义函数、GROUP_CONCAT、JSON 运算、特殊索引、生成列和 ON UPDATE 自动行为会明确报错。ENUM/SET 仅保留字符串值；MySQL 排序规则不复制到 Oracle。空间类型保存原始 BLOB，不具备 Oracle Spatial 语义。业务触发器、函数、存储过程继续使用原样导出功能。

名称检查遵循 [Oracle 11g 对象名称规则](https://docs.oracle.com/html/E10592_02/sql_elements008.htm)，除长度和大写冲突外，也拒绝 Oracle 不允许的双引号和 NUL。
