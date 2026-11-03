# MySQL → 达梦触发器迁移

新建含结构的 MySQL → 达梦任务默认开启“迁移所选表的触发器”，可以取消。仅对象任务可单独选择触发器；旧任务保持已保存的选项。批量任务可按迁移组合使用默认值，或统一开启、关闭 MySQL 触发器迁移。

触发器在历史数据导入、行数检查及启用的内容校验完成后创建。仅数据任务不会创建触发器，且会在写入前检查目标表：存在启用或状态不明的业务触发器时跳过该表并报告原因。工具不会自动停用业务触发器。

## 自动转换范围

- BEFORE/AFTER INSERT、UPDATE、DELETE 行触发器，合法的 NEW/OLD 引用及 BEFORE 赋值。
- 标量局部变量、嵌套作用域、IF、CASE、WHILE、REPEAT、LOOP 及合法标签跳转。
- 单表 INSERT、UPDATE、DELETE，明确列清单的 INSERT SELECT。无列名的单行 `INSERT INTO t VALUES (...)` 按源表元数据中的可见列顺序补齐列名，并应用目标表名、列名映射；仍校验列数、赋值类型、自增列、生成列及依赖。同源库限定名（如 `epoint_zhdc.frame_attachstorage_ban`）映射到当前目标 schema。
- SELECT INTO 局部变量及基本 COUNT/SUM/AVG/MIN/MAX 聚合；查询零行保持变量原值，多行报错。
- 有明确类型的数值、逻辑和白名单字符串、时间函数。
- 完整 SET 赋值右侧的 `UUID()` 和 `REPLACE(UUID(), '-', '')`，可带普通括号或无 FROM 的单表达式 SELECT 包装。前者用局部变量保存单次 `SYS_GUID()` 的结果，再拼为 36 位小写、带横杠的字符串；后者保持原有 `LOWER(RAWTOHEX(SYS_GUID()))` 转换及 32 位小写十六进制格式。仅保留唯一标识用途，不保留 MySQL UUID v1 的时间/版本语义；不依赖目标将空字符串视为 NULL 与否。复杂表达式或其他语句上下文中的 UUID 仍需人工适配。
- SIGNAL SQLSTATE '45000' 的业务异常；目标错误号改为达梦应用错误号。

同表、同时机、同事件的触发器通过 ACTION_ORDER 和达梦 FOLLOWS 保留顺序。前序未安装时，后续记录依赖阻塞，其他独立触发器继续处理。

源库对象名和 DDL 保留原始大小写；生成 SQL 使用目标实际的大写表名及已有列改名映射。解析使用触发器保存的 SQL_MODE。目标 NULL/空字符串、字符排序和隐式转换不能保持语义时，该触发器记录为不支持，不会执行部分正文。不修改达梦实例兼容参数。

游标、异常处理器、动态 SQL、会话变量、跨库引用、未支持的查询和自定义函数/存储过程调用需要人工适配。函数和存储过程仍使用原始 SQL 导出功能，不参与跨库自动转换。

## 创建结果与重试

每个触发器是一条完整 DMSQL CREATE，不拆分内部语句。只有目标目录确认定义、事件、表、编译有效性和启用状态均正确，才统计成功。完全一致的已有触发器可复用，同名不同定义对象不会被覆盖。

报告保留源 DDL、转换 DDL、失败位置与阶段。已生成的修复计划不可编辑，重试前重新核对依赖和目标实际对象。执行结果未知时，定义一致有效则确认复用；明确不存在才执行原冻结 CREATE；冲突或无法核实保持未知。不支持语法产生的诊断记录不具备执行权限。

升级转换器后，旧的不支持语法诊断不会自动变为可执行计划。可新建仅触发器迁移任务，对所选表重新读取元数据并生成计划；已有成功计划的 SQL 输出和版本保持兼容。

触发器按目标所属用户权限执行，不映射源 DEFINER。两端业务会话应保持一致时区。

## 验证

常规验证：

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./...
/Users/kay/sdk/go1.25.5/bin/go build ./...
cd frontend
npm test
npm run build
```

实库测试必须指向独立空测试 schema，配置 `MYSQL_DM_TEST_SOURCE_DSN`、`MYSQL_DM_TEST_TARGET_DSN`、`MYSQL_DM_TEST_SCHEMA` 和 `MYSQL_DM_TRIGGER_ALLOW_DDL=1` 后运行：

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestMySQLDamengTriggerIntegration$' -count=1 -v
```

夹具创建唯一名称的表和触发器，覆盖同库限定名的无列名 INSERT、两种 UUID 格式、历史值保留及新值生成，检查历史导入无审计副作用，并比较迁移后增删改、触发顺序、局部查询与业务异常的实际结果。测试完成后清理夹具对象。
