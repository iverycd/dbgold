# Oracle → 达梦集成验证

`TestOracleDamengIntegration` 是显式启用的真实数据库测试，默认跳过。测试需要 Oracle 11g 或更新版本、DM8，以及两个**初始没有表的独立测试 schema**。不要求 `COMPATIBLE_MODE=2`；测试读取目标现有模式并按实际能力验证，不创建 schema、不修改数据库参数，也不使用开发或生产连接配置。

当前没有随代码提供可用的 Oracle/DM 实例凭据。未配置环境而跳过测试，或模拟测试通过，均表示**实库未验收**。

## 配置与运行

在本机通过环境变量提供测试连接，不要将密码提交到代码库：

```sh
export ORACLE_DM_TEST_SOURCE_DSN='oracle://test_user:password@oracle-host:1521/service'
export ORACLE_DM_TEST_SOURCE_SCHEMA='ORACLE_DM_TEST'
export ORACLE_DM_TEST_TARGET_DSN='dm://test_user:password@dm-host:5236'
export ORACLE_DM_TEST_TARGET_SCHEMA='ORACLE_DM_TEST'
export ORACLE_DM_TEST_ALLOW_DDL=1
# 可选：锁定本轮验收所用实例模式，避免误连其它矩阵环境。
export ORACLE_DM_TEST_EXPECT_MODE=0
# 可选专项：已有 Oracle DIRECTORY；仅迁移定位信息，不读取外部文件。
# export ORACLE_DM_TEST_BFILE_DIRECTORY='DBGOLD_TEST_DIR'
# 可选专项：两端已安装、授权原生空间组件。
# export ORACLE_DM_TEST_SPATIAL=1

/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestOracleDamengIntegration$' -count=1 -v
```

DSN 格式沿用相应驱动。需要源库读取当前 schema 元数据及创建表、索引、视图、序列、触发器和注释的权限，以及原生 XMLTYPE；目标库需要读取 `V$DM_INI`、目标元数据，以及在指定测试 schema 中创建和删除相同对象的权限。源库和目标库的字符集需能保存测试中的中文。生产迁移只按所选对象读取必要能力；本测试为了记录验收矩阵会额外读取实例模式。

BFILE 专项要求两端已有同名 DIRECTORY 并具备相应权限，只核对目录与文件名定位信息，不迁移目录对象或外部文件。Oracle 数据写入在每页执行前完成数值和时间转换，以及按实际目标列字符数、物理字节容量和目标编码的检查；非法值或超限值拒绝整页写入。`MY_STRICT_TABLES(7)` hint 仅保留为附加保护，不作为正确性依据；不再用非法字符串的 `SELECT CAST` 结果判定 INSERT 是否安全，也不修改会话或实例参数。

只有四项连接/schema 设置齐全且 `ORACLE_DM_TEST_ALLOW_DDL=1` 时才会创建对象。测试首先检查两端 schema 没有表，然后创建 `ODM<时间戳>` 前缀的唯一对象；结束时仅删除这些测试对象。进程被强制中止时，需要手动清理对应前缀的表、视图和序列，重新运行前恢复空测试 schema。

## 验证内容

- 先迁移结构，再按先父表后子表的顺序执行仅数据迁移（已有外键的依赖顺序由调用方安排）；清理目标测试对象后独立执行结构和数据迁移。
- Oracle `DATE` 的时分秒、`TIMESTAMP(6)`、本地时区时间的 UTC 瞬间、`NUMBER(38,18)`、负的两类 interval，以及 CLOB/NCLOB/BLOB 的值与大小写。模式 2 的 DATE 映射为目标 DATE，其余模式映射为 TIMESTAMP(0)，两者都必须保留时分秒。
- NCHAR/NVARCHAR2 的中文、大小写和字符长度单位；目标字符 DDL 使用显式 `n CHAR`，不依赖实例默认长度单位。
- FLOAT、BINARY_FLOAT、BINARY_DOUBLE 的实际值：FLOAT 依据目标原生数值族使用十进制或二进制参数，允许目标浮点类型正常舍入；NUMBER 仍要求精确绑定。
- LONG 文本及 ROWID/UROWID：ROWID 使用 18 字节规范 ASCII，UROWID 使用源端显式文本投影后的原始字节；目标不把二进制列当成新的物理行定位器。
- 单独迁移 XMLTYPE：有原生能力时核对 XML 内容；缺少能力时要求在建表前报告失败，不能悄悄降级成 CLOB。
- 显式配置 DIRECTORY 后验证 BFILE 目录/文件定位信息，不打开或读取外部文件；显式启用空间测试后验证 MDSYS 二维点到 SYSGEO2 的坐标及 SRID。
- 主键、降序普通索引、外键、表和列注释；随后通过视图迁移入口验证 `NVL` 及表引用映射。
- 单一 `BEFORE INSERT` 序列赋值触发器及目标续号，要求目标新值不低于源序列安全后继值并保留步长。
- 单独迁移 `TIMESTAMP(9)` 表：使用只读往返表达式验证目标实际保留九位小数秒的能力；有能力时核对数据，无能力时必须在创建目标表之前拒绝。结果不单独由 `IFUN_DATETIME_MODE` 的参数值推断，测试也不会修改它。

Oracle 版本认证需要在实际 Oracle 11g 和实际较新版本各执行一次；较新版本的兼容设置不能替代 11g 分页验证。单元测试和默认跳过的集成测试不能证明已通过真实数据库验收。

模式矩阵至少在 `COMPATIBLE_MODE=0`、`2`、`4` 的独立现有实例上各运行一次，并分别记录 Oracle/DM 版本、实际模式、NUMBER_MODE、IFUN_DATETIME_MODE 和输出日志。不同模式通过修改目标连接并设置 `ORACLE_DM_TEST_EXPECT_MODE` 运行；测试不会切换或重启实例。IFUN_DATETIME_MODE 的两种状态也要独立记录，不能以一种配置替代另一种。BFILE 外部文件定位及空间类型使用上述可选环境变量在具备相应组件和权限的专用实例验证；测试不创建外部目录、不读取文件内容、不安装空间组件。专项未启用时单独输出跳过并标记未验收，不能由普通表测试通过替代。

## 只读字符探针验证（2026-10-20）

`TestOracleDamengReadOnlyCharacterCapabilities` 仅执行 SELECT，需要 `ORACLE_DM_TEST_TARGET_DSN`，可选 `ORACLE_DM_TEST_TARGET_SCHEMA`；不需要 Oracle 连接或 `ALLOW_DDL`，不会创建或修改任何对象：

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestOracleDamengReadOnlyCharacterCapabilities$' -count=1 -v
```

已在 DM8 构建 `03134284294-20240919-243448-20119` 的现有模式 4 实例上完成只读对照。实际参数为 `LENGTH_IN_CHAR=0`、`NVARCHAR_LENGTH_IN_CHAR=1`、`MY_STRICT_TABLES=1`、`SF_GET_UNICODE_FLAG()=1`（UTF-8）。

| 只读对照 | 观察结果 |
|---|---|
| 直接绑定“汉A” | 完整返回 2 字符、4 字节 |
| VARCHAR/NVARCHAR 的 `CAST(... AS ...(2 CHAR))`，参数及字面量 | 表达式均返回空串，LENGTH/LENGTHB 为 0 |
| NCHAR 的短 CAST | 补成两个空格，内容字符已丢失 |
| NVARCHAR 的 `32 CHAR` 缓冲区及 NCHAR 同宽缓冲区去补空格 | 服务端长度/完整字节检查均通过，修正后的实际 Go 能力检查通过 |
| `SELECT /*+ MY_STRICT_TABLES(7) */ CAST(... AS INT)` 的非法数值输入 | 返回 0；该参数在本实例的 `V$HINT_INI_INFO` 中没有记录 |
| 已有国家字符列目录（仅读元数据） | CHAR_LENGTH=5、DATA_LENGTH=20、CHAR_USED=B、SYSCOLUMNS.SCALE=7；有效单位应识别为字符 |

短 CAST 的长度行为不能代替真实列容量。国家字符探针现在只验证语法与足够缓冲区内的绑定内容；NCHAR 的定长补空格单独处理。写入容量依据真实目标列目录，字符单位也检查目标编码后的物理字节数。BFILE 的目录/文件引用不按原生定位器的 DATA_LENGTH 限制。

本次只读诊断没有执行建表或数据写入，**不能替代模式 0/2/4 的完整迁移及字符单位建表验收**；该验收矩阵仍需在专用空 schema 中运行上面的 DDL/DML 测试。

## 视图自动转换边界

Oracle 到达梦视图使用专用转换器映射已验证的 schema、表、列及别名，不经过 PostgreSQL 或 SQL Server 解析器。支持常规 SELECT、显式 JOIN、过滤、分组、聚合、UNION、CASE、CAST、常用 Oracle 内置函数和可绑定的通配符。Oracle 空字符串及依赖目标能力的表达式按专用兼容规则处理；不支持或不能证明语义的表达式在视图对象报告中明确失败，不因模式不是 2 阻止其它表迁移。

使用拼接、空串结果函数、填充、SOUNDEX、LAST_DAY、整数除法和 CAST AS DATE 的视图，仅按实际用到的特性执行只读常量验证。不同语义只阻止当前视图；SUBSTRB 暂不跨字符集猜测字节位置。默认值与视图的字符串保持内容大小写，空字面量转换为 NULL；含反斜杠的 SQL 字面量目前明确要求人工适配，避免目标转义设置改变内容。普通参数化数据中的反斜杠不受此限制。

FLOAT 列默认值目前只自动接受数值常量与 NULL，并在建表前检查目标实际浮点范围；其他表达式保留人工适配。CAST AS DATE 默认值因格式和小数秒截断语义不能确认而明确拒绝，可以使用显式格式的 TO_DATE。XMLTYPE 保留 XML 内容语义，不能承诺原始排版或 XML Schema 存储属性；自定义 DTD 实体可能被当前 XML 校验器明确拒绝。空间数据仅自动转换受支持的普通二维线性几何，缺少 SRID、其他维度、曲线和未验证的空间子类会明确报错。

CTE、子查询、CONNECT BY、`(+)`、NATURAL/USING JOIN、跨 owner 引用、数据库链接及用户/包函数不自动转换，必须给出明确错误。复杂表达式必须具有可确定的输出别名；列名歧义和目标大写命名冲突同样拒绝。

## 数值和时间边界

NUMBER 目标值转换会拒绝超出 DM 可保真范围的值：有效十进制位超过 38 位、负 scale、scale 大于 precision 的 Oracle NUMBER 不在自动映射范围内。DM 有效数值指数范围也小于 Oracle，因此极小/极大 NUMBER 可能在读取数据阶段报错，单靠列元数据预检不能排除全部值越界。FLOAT 系列按照批准的类型映射允许正常目标浮点舍入，但仍拒绝非有限值及溢出；不能把 FLOAT 的允许舍入用于 NUMBER。

当前使用的 dm 驱动 1.8.23 对某些负的 38 位有效数字且规范化小数位为奇数的 decimal 存在传输截断问题，转换器会明确拒绝这些值；37 位有效数字或受支持的偶数小数位不受该保护限制。公元前日期和 `1582-10-05` 至 `1582-10-14` 也会明确拒绝，避免驱动或目标日历规则悄悄改变日期。此类数据错误按现有迁移失败及修复流程报告，不能视为已保证所有数据在目标写入前完成值级验证。

驱动对部分日秒区间的小数秒也存在解析舍入问题（例如 `.000249`）；转换器检查实际绑定值，不能精确保留时报告数据失败，不静默改值。

## 关联编号与恢复

仅迁移所选表关联的序列，不迁独立业务序列。支持单一序列赋值的 `BEFORE INSERT FOR EACH ROW` 触发器（无条件赋值或仅 NULL 时赋值），以及 `BY DEFAULT ON NULL` 身份列。`ALWAYS`、`BY DEFAULT`、普通 `DEFAULT seq.NEXTVAL`、复杂触发器和循环序列不自动转换。

序列起值、步长和边界必须能由 DM int64 序列表达；Oracle 默认的大范围 MAXVALUE 可能因此报告不支持，不自动钳制边界。同名目标序列或触发器已存在时不覆盖，应先核对现有对象，再选择仅数据迁移或人工处理。续值基于只读缓存水位和所选关联列极值，不消费源 NEXTVAL，缓存序列可产生安全的编号间隙。

仅数据和补迁不会禁用触发器。启用的无条件编号触发器或业务触发器可能改变源值，会明确阻止复制；工具生成的仅 NULL 时编号触发器只在源绑定列为 NOT NULL 时放行。恢复前重新检查目标类型容量、时间精度及实例能力，避免手动改表后静默窄化。数据失败只回滚当前分页，此前已提交分页保留。
