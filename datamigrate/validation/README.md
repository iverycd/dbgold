# 迁移后数据内容校验

普通迁移和批量迁移通过 `validate_data: true` 启用，默认关闭。仅结构、仅对象、仅视图和增量任务不启用。关闭时不进行额外内容扫描；历史任务的新存储字段默认 `false`。

启用后先完成现有迁移与行数检查，再按迁移前冻结的表、列和主键，以及实际目标列名映射进行完整内容比对。仅数据迁移也检查整个目标表，已有记录会计入结果。源库和目标库必须在迁移、校验期间保持停写和停止结构变更；这里不建立跨库一致性快照。

## 一致标准

- 原值扫描独立于迁移的 Reader 清洗和 Writer 值转换，能够发现 NUL 清洗、时间截断等损失。
- NULL、空串、尾空格、NUL、二进制内容分别保留；精确数值忽略 `1` / `1.00` 等表示差异。浮点按实际二进制精度比较，不设置容差。
- 无时区时间按完整时间字段比较；带时区时间按 UTC 瞬间比较。MySQL TIMESTAMP 沿用迁移连接的会话时区，不在校验时另行改变其墙上时间。
- 明确的 JSON 列允许对象键顺序和数值表示差异，数组顺序仍有意义。重复 JSON 对象键不静默折叠，而是标记无法校验。
- SQL Server 的 NCHAR / NVARCHAR / NTEXT 以原始 UTF-16LE 字节回读，严格检查代理项配对；合法的 emoji、NUL、尾空格和 U+FFFD 均保留。非 Unicode CHAR / VARCHAR / TEXT 若驱动结果含 U+FFFD，则无法区分原始字符与驱动的替换行为，保守标记无法校验；这也会拒绝其中原本合法的 U+FFFD。
- 未知复杂类型、缺少无损回读能力、无法确认时区语义或超出资源限额时，明确报告无法完成校验，不跳列、不退化为抽样。

## 执行与结果

每表两端分别使用单个游标和显式列投影读取。主键与整行内容生成 SHA-256 摘要，通过最多 64 MiB 排序缓冲、32 路归并比较；表之间顺序执行。有主键时识别缺失、多余、字段变化和重复键；无主键时按整行摘要及重复次数比较。

临时文件只存固定长度摘要，目录权限 `0700`，文件权限 `0600`，所有返回路径均清理。内存还包括驱动当前行、元数据和有限差异样例；当前扫描器单行限额为 8 MiB，超限明确失败。存在差异时至多再次回读两端一次，保存最多 20 个样例；文本展示至多 256 字符，大字段和二进制展示长度与摘要。

报告新增可选 `dataValidation` 区块。未启用时缺省，不代表通过。`passed` 表示完整扫描后的摘要高置信度一致；`mismatch`、`error`、`unsupported`、`incomplete` 均使任务失败，已迁移数据保留。主动取消沿用任务 `cancelled` 状态。没有任何可校验表的任务不标记内容通过。

神通通过扩展的 JDBC 元数据和游标协议回读，fetch 同时限制行数和字节数。Java 源码与桥协议须一起发布；构建命令为 `sh jdbcbridge/java/build.sh`。

## 验证

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./...
/Users/kay/sdk/go1.25.5/bin/go build ./...
```

MySQL 与 PostgreSQL 实际迁移测试使用专门的可丢弃实例，测试会建立并清理独立命名空间：

```sh
DBGOLD_VALIDATION_MYSQL_DSN='...' DBGOLD_VALIDATION_PG_DSN='...' \
  go test ./datamigrate -run 'TestContentValidation.*Integration' -v -count=1
```

原值扫描集成测试使用 `VALIDATION_MYSQL_DSN` / `VALIDATION_POSTGRES_DSN`，覆盖超 int64 整数、高精度小数、部分零日期、非 UTC TIMESTAMP、二进制和 BIT。缺少 DSN 时跳过；跳过不等于对应数据库已实库验收。Oracle、达梦、SQL Server、神通及各 PG 兼容产品仍需各自环境的实库验收，不能仅凭 PostgreSQL 测试推广认证。
