# Oscar 目标对象命名与升级说明

- Oscar 迁移对象统一大写，优先于 `lower_case_names`。源端查询、筛选继续使用原始大小写，所选目标 Schema 和 Owner 名称保持原值。
- 普通/唯一索引：`<大写表名>_<大写源索引名>`。例如两个表的 `IDX_XZLXGUID` 分别创建为 `DZT_ZNGW3_HTXZXX_IDX_XZLXGUID`、`DZT_ZNGW3_HTTSCVER_IDX_XZLXGUID`。
- 主键：`PK_<大写表名>`；序列：`SEQ_<大写表名>_<大写列名>`。报告、DDL 和 Owner 修改使用相同名称。
- 创建目标对象前检查源对象转大写后的命名冲突。不会自动截短名称、忽略索引冲突或删除其他表上的索引；服务器报告的名称长度限制和 JDBC 错误保留在迁移结果中。
- 视图的标识符引用同步转换，字符串和注释不参与大小写转换；不支持的源表达式仍可能执行失败，需根据报告中的完整 DDL 手动适配。

## Owner 选项

单任务选择 Oscar 后默认不修改 Owner，可手动勾选；其他目标库保留原默认值。单任务 API 的 `change_owner` 未传时 Oscar 默认 `false`、其他目标默认 `true`，显式值优先。

批量页为 Oscar 提供单独开关，默认关闭，其他目标继续使用原开关。批量 API 对 Oscar 按以下顺序取值：

1. 显式 `oscar_change_owner`。
2. 兼容旧客户端显式传入的 `change_owner`。
3. 两者均缺省时为 `false`。

非 Oscar 目标忽略 `oscar_change_owner`。历史任务保存的选项不作改写。

## 重新迁移注意事项

此变更不自动重命名或清理历史小写对象。建议使用新 Schema，或自行确认并清理旧对象后重新迁移；已有大写表仍按当前迁移模式重建。仅数据/仅对象迁移也使用大写目标名称，不能直接用于补写历史小写表。

仅对象迁移重复创建同一个最终索引仍会明确报错，不会用 `IF NOT EXISTS` 将可能不同的索引定义当成成功。

## 验证

运行 `go test ./...`、`go vet ./...`，以及前端目录内的 `npm test`、`npm run build`。
配置 `OSCAR_TEST_URL`、`OSCAR_TEST_USER`、`OSCAR_TEST_PASSWORD`、`OSCAR_TEST_SCHEMA` 和 JDBC 运行时后，可运行 `go test ./datamigrate/target -run TestOscarIntegrationCapabilityProbe -v` 验证真实数据库能力。
