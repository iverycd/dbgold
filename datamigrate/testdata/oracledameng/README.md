# Oracle → 达梦集成验证

`TestOracleDamengIntegration` 是显式启用的真实数据库测试，默认跳过。测试需要 Oracle 11g 或更新版本、DM8 Oracle 兼容模式（`COMPATIBLE_MODE=2`），以及两个**初始没有表的独立测试 schema**。它不创建 schema，不修改数据库兼容参数，也不使用开发或生产连接配置。

## 配置与运行

在本机通过环境变量提供测试连接，不要将密码提交到代码库：

```sh
export ORACLE_DM_TEST_SOURCE_DSN='oracle://test_user:password@oracle-host:1521/service'
export ORACLE_DM_TEST_SOURCE_SCHEMA='ORACLE_DM_TEST'
export ORACLE_DM_TEST_TARGET_DSN='dm://test_user:password@dm-host:5236'
export ORACLE_DM_TEST_TARGET_SCHEMA='ORACLE_DM_TEST'
export ORACLE_DM_TEST_ALLOW_DDL=1

/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestOracleDamengIntegration$' -count=1 -v
```

DSN 格式沿用相应驱动。需要源库读取当前 schema 元数据及创建表、索引、视图、序列、触发器和注释的权限；目标库需要读取 `V$DM_INI`、目标元数据，以及在指定测试 schema 中创建和删除相同对象的权限。源库和目标库的字符集需能保存测试中的中文。

只有四项连接/schema 设置齐全且 `ORACLE_DM_TEST_ALLOW_DDL=1` 时才会创建对象。测试首先检查两端 schema 没有表，然后创建 `ODM<时间戳>` 前缀的唯一对象；结束时仅删除这些测试对象。进程被强制中止时，需要手动清理对应前缀的表、视图和序列，重新运行前恢复空测试 schema。

## 验证内容

- 先迁移结构，再按先父表后子表的顺序执行仅数据迁移（已有外键的依赖顺序由调用方安排）；清理目标测试对象后独立执行结构和数据迁移。
- Oracle `DATE` 的时分秒、`TIMESTAMP(6)`、`NUMBER(38,18)`、负的两类 interval，以及 CLOB/NCLOB/BLOB 的值与大小写。
- 主键、降序普通索引、外键、表和列注释；随后通过视图迁移入口验证 `NVL` 及表引用映射。
- 单一 `BEFORE INSERT` 序列赋值触发器及目标续号，要求目标新值不低于源序列安全后继值并保留步长。
- 单独迁移 `TIMESTAMP(9)` 表：`IFUN_DATETIME_MODE=1` 时核对九位小数秒；其他值时必须在创建目标表之前拒绝。测试不会自动开启此参数，因此建议分别对两种目标配置运行。

Oracle 版本认证需要在实际 Oracle 11g 和实际较新版本各执行一次；较新版本的兼容设置不能替代 11g 分页验证。单元测试和默认跳过的集成测试不能证明已通过真实数据库验收。

## 视图自动转换边界

Oracle 到达梦视图保留原始 Oracle 表达式和字面量，只映射已验证的 schema、表、列及别名；不经过 PostgreSQL 或 SQL Server 解析器。支持常规 SELECT、显式 JOIN、过滤、分组、聚合、UNION、CASE、CAST、常用 Oracle 内置函数和可绑定的通配符。字符串（包含 q 引号）及注释保持原文。

CTE、子查询、CONNECT BY、`(+)`、NATURAL/USING JOIN、跨 owner 引用、数据库链接及用户/包函数不自动转换，必须给出明确错误。复杂表达式必须具有可确定的输出别名；列名歧义和目标大写命名冲突同样拒绝。

## 数值和时间边界

当前目标值转换会拒绝超出 DM 可保真范围的值：有效十进制位超过 38 位、负 scale、scale 大于 precision 的 Oracle NUMBER 不在自动映射范围内。DM 有效数值指数范围也小于 Oracle，因此极小/极大 NUMBER 可能在读取数据阶段报错，单靠列元数据预检不能排除全部值越界。

当前使用的 dm 驱动 1.8.23 对某些负的 38 位有效数字且规范化小数位为奇数的 decimal 存在传输截断问题，转换器会明确拒绝这些值；37 位有效数字或受支持的偶数小数位不受该保护限制。公元前日期和 `1582-10-05` 至 `1582-10-14` 也会明确拒绝，避免驱动或目标日历规则悄悄改变日期。此类数据错误按现有迁移失败及修复流程报告，不能视为已保证所有数据在目标写入前完成值级验证。

驱动对部分日秒区间的小数秒也存在解析舍入问题（例如 `.000249`）；转换器检查实际绑定值，不能精确保留时报告数据失败，不静默改值。

## 关联编号与恢复

仅迁移所选表关联的序列，不迁独立业务序列。支持单一序列赋值的 `BEFORE INSERT FOR EACH ROW` 触发器（无条件赋值或仅 NULL 时赋值），以及 `BY DEFAULT ON NULL` 身份列。`ALWAYS`、`BY DEFAULT`、普通 `DEFAULT seq.NEXTVAL`、复杂触发器和循环序列不自动转换。

序列起值、步长和边界必须能由 DM int64 序列表达；Oracle 默认的大范围 MAXVALUE 可能因此报告不支持，不自动钳制边界。同名目标序列或触发器已存在时不覆盖，应先核对现有对象，再选择仅数据迁移或人工处理。续值基于只读缓存水位和所选关联列极值，不消费源 NEXTVAL，缓存序列可产生安全的编号间隙。

仅数据和补迁不会禁用触发器。启用的无条件编号触发器或业务触发器可能改变源值，会明确阻止复制；工具生成的仅 NULL 时编号触发器只在源绑定列为 NOT NULL 时放行。恢复前重新检查目标类型容量、时间精度及实例能力，避免手动改表后静默窄化。数据失败只回滚当前分页，此前已提交分页保留。
