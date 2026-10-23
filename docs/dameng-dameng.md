# 达梦 → 达梦：Schema 全对象迁移

在迁移页选择达梦源连接和达梦目标连接，选择一个源 Schema 和一个已存在的目标 Schema，即进入原生迁移流程。使用 Go 达梦驱动读取目录、回放原生定义和复制数据，不依赖 `dexp` / `dimp`，不走跨厂商类型映射或过程语法转换。

此处“全部对象”指所选业务 Schema 的完整对象清单和数据。用户、角色、表空间、实例参数、实例级作业和公共对象不由本任务创建；对象引用的外部 Schema、表空间及授权主体需事先在目标端准备。源目标可以是同一实例的不同 Schema；同一实例的同一 Schema 一律拒绝。

## 使用条件和行为

- 源目标必须是相同 DM8 版本及构建，兼容模式、字符集、大小写规则、页大小等已读取的语义参数必须一致。不能把本功能用于升级或兼容模式转换。
- 在开始预检前暂停源端业务写入，包括序列/自增消费；迁移、核对结束前保持源端及目标迁移范围停写。本功能不提供 CDC，也不建立跨表一致性快照。
- 目标 Schema 需手工创建；迁移账号需能读取完整源目录和原始 DDL，并具有目标对象创建、写入及所选覆盖操作的权限。无法读取必要元数据也会阻止迁移。
- 只提供整个 Schema 的“全部对象 + 数据”流程。不支持指定表、排除对象、仅结构、仅数据，或通过视图/对象页单独发起达梦同库任务。
- 保留对象和列的目录大小写、达梦类型、默认值及原生过程体。Schema 改名仅替换 SQL 标识符中的源 Schema；字符串常量及动态 SQL 字符串不改写。预检中出现相关提示时，须核对应用和过程内的字符串引用。

目录盘点结合原生字典与兼容视图，覆盖可提取原生定义的表、表数据、主键/唯一/检查/外键约束、普通及全文索引、分区归属、注释、独立序列、自增列、视图、普通触发器及包括 TIMER、CHECKPOINT 在内的事件触发器、函数、过程、包及包体、类型/类及其主体、域、私有同义词、私有 DBLink、对象/列授权和可处理的物化视图。由父对象创建的内部对象仍保留在清单中，并按父对象结果核对。

任务先创建基础对象，再复制数据，随后建立外键、业务触发器和其他依赖对象，恢复可读取的启停状态并编译核对。复制数据期间不运行新迁移的业务触发器。物化视图采用已存储的快照数据，避免用重新查询源基表代替快照迁移。

序列按源目录 `LAST_NUMBER` 的缓存边界续号；预检不会调用源 `NEXTVAL`。缓存序列可能留下原本允许的编号间隙。自增列复制原 ID，并按源种子、步长和实际当前值计算安全下一值；为续号而改变的目标种子会记录在报告中。空表的公开只读状态无法区分“从未使用首值”和“首值已生成后删除”，此时按当前值加步长继续并明确警告，避免复用已消费的值，可能跳过尚未使用的首值。

## 预检、覆盖和失败处理

点击“预检并查看对象计划”可查看完整对象清单、同名冲突、删除重建清单和阻止原因。正式任务会在目标 Schema 锁内重新预检，不执行浏览器提交的 SQL，也不以旧预检结果跳过检查。

“覆盖目标同名对象”默认关闭：发现冲突即阻止本次任务，尚未执行目标 DDL 或数据写入。开启后只删除并重建计划内的同名冲突对象，保留无关目标对象；若目标独有对象依赖待覆盖对象，则阻止覆盖。批量页的覆盖开关仅应用于达梦同库行，每行执行前仍重新预检。

覆盖后的 DDL 和分批数据提交不构成整个 Schema 的单一事务。执行期故障可能留下已完成对象及部分数据；报告保留对象清单、原始 SQL、失败阶段、行数与原生定义核对结果。需要检查数据内容时，可启用单独的数据内容校验，其失败/不支持结果也不能当作校验通过。

原生对象修复仅重试原任务冻结的完整 SQL 单元，过程体只读，已成功单元不会重新执行。数据接续先核对冻结表结构和当前数据状态；非空目标必须明确确认仅清空该表后完整重导。执行结果未知时禁止盲目重放，应先核对目标实际状态。

表定义同时冻结约束实际使用的用户索引、约束启停及 `VALIDATE` / `NOVALIDATE` 状态。用户索引先于引用它的主键/唯一约束创建；用户命名的独立聚集索引使用原始 `CREATE CLUSTER INDEX` 恢复名称和排序。允许保留历史不符合检查或唯一条件的 `ENABLE NOVALIDATE` 约束在复制数据后恢复。命名约束、索引和临时承载表中的显式名称同样参与冲突检查，不能占用目标保留表的名称，也不能在覆盖表时静默删除目标独有的命名约束。此类表内独立执行单元补迁时逐个核对目标目录、定义、归属和状态，表已存在不能代替子单元成功证明。

## 物化视图、事件触发器和 DBLink

`COMPLETE` / `NEVER` 物化视图直接复制已存储的快照，允许快照陈旧。`FAST` / `FORCE` 同时冻结每个视图的实际快照、刷新水位和完整增量日志。共享日志的多个视图组成一个恢复组，按源刷新水位顺序在目标建立本地基线，再恢复各自的旧快照及尚未消费的日志；源端不执行刷新。原生日志顺序和每个视图已经消费的范围分别映射，迁移后实际 FAST 可继续消费旧日志及目标新写入。

普通承载表及 RANGE / LIST 叶分区通过 SQL 分区交换恢复快照；HASH 分区承载表使用 `SYS.DBMS_REDEFINITION` 的 ROWID 模式装载原物理存储，随后按原生目录中的绑定关系恢复名称。两条路径均只使用现有 Go 驱动执行 SQL，保留承载表分区、索引、物理绑定及目标刷新水位，不依赖 FLDR 动态库、客户端命令或系统目录写入。HASH 路径的目标账号需有该系统包的执行权限，临时名称也参与只读冲突检查。

当前基表的源 ROWID 与插入时返回的目标 ROWID 在同一事务中保存映射；已删除行的历史 ROWID 也保持一致映射。输出中的直接 ROWID 随目标物理地址重建，其他列、重复行数量及各视图旧快照内容逐项核对。不可变快照与日志暂存保留在冻结计划中；历史组执行结果未知时先核对目录，再从不可变输入完整重建，不能反向重放已经完成的交换。全部内容和状态核对成功后，才恢复原 `FAST` / `FORCE`、刷新时机及调度配置。

基表已经删空的旧快照仍保留原数据。目标通过事务内插入并删除临时行恢复原生变化状态，退出前恢复冻结的约束、非空属性、默认值及自增续号；不会留下已提交的临时行或消费默认值引用的序列。空表上的独立表达式/虚拟列索引在历史恢复后创建，避免临时行触发表达式求值。值相等但源状态为 `NEEDS_COMPILE` 的视图仍保留该状态；原 `NEEDS_FULL_REFRESH` 也保留，后续 FORCE 继续按原生规则回退完全刷新。没有增量日志的合法 FORCE（包括 UNION ALL）保留旧快照，后续 FORCE 可正常执行。

空基表的显式可写分区键从原生完整分区定义冻结候选值，以只读 `PARTITION FOR` / `SUBPARTITION FOR` 查询确认落入既有叶分区，覆盖负数、精确边界、日期及多层分区。探测不会执行源端 INSERT、创建新的 INTERVAL 分区或调用用户函数。合法生成分区键保留原有临时行路径，由目标原生表达式计算。`BUILD DEFERRED` 视图保持空快照、原生 `UNUSEABLE` 状态和未初始化水位，核验不会通过额外刷新将其转为已构建状态。

原生 TABLE 元数据遗漏的已有自动 INTERVAL 分区另行冻结，包含名称、边界、表空间及物理定义。目标在导入数据前通过受控临时行物化这些分区，恢复原名称并核对完整目录，保留间隔之间原本不存在的空洞。完整计划先持久化，准备失败或取消不会继续复制数据；未知结果先核对目标分区和临时属性恢复状态。

生成列作为 INTERVAL 分区键时，已支持常量和经过实测的精确数值仿射表达式，包括多列线性组合、定点数刻度及负系数。提取过程只解析原生表达式，以精确有理数和整数格点求取真实可写输入，再通过原生 CAST 和只读分区路由核验；目标仅插入这些真实输入，由原生表达式计算生成列。每个分区的多列种子参与冻结指纹、Schema 映射和账本重载。不会为求逆调用源用户函数。

分区父表的 `SYSCOLUMNS.INFO2` 可能没有自增标志，自增识别同时读取原生列声明；不会把默认值、字符串或其他列中的 `IDENTITY` 文本当作自增属性。临时行对自增列使用明确的非空值，恢复原始定义后再复制原 ID，最终按源安全边界续号。

分区自增的公开 `IDENT_CURRENT` 也可能落后于已分配的值。读取根表原生 `INFO6` 指向的唯一活动序列，并核对列 ID、序列类型及原生父链，再沿原步长级数越过公开状态和现存数据边界；不会把历次 `DROP/ADD IDENTITY` 留下的旧内部序列当作当前水位。整个提取过程只读，不消费源自增值。

本次构建的绑定 INSERT 即使传入非空值，也可能额外计算 `DEFAULT ON NULL` 序列表达式。原生列计划因此冻结原始默认表达式，数据批次在事务外暂时移除该属性，提交或回滚后恢复并核对定义，保留实际存储的 NULL 和非 NULL 值。数据提交/回滚响应失败时，先独立确认服务端事务回滚，才允许恢复 DDL；确认失败就丢弃连接，避免隐式提交部分数据。整表补迁能恢复中断留下的缺省属性，当前源为零行时同样执行核对与恢复。

本次测试的 DM8 构建在完全原生的双视图极速创建/刷新中也复现了时间边界处误清增量日志。历史恢复在每次目标基线及结束前等待服务端进入下一秒的稳定时间段，并支持取消和超时；实库夹具也单独等待源端水位，避免先构造已经受该引擎问题影响的日志。该处理是迁移过程的规避措施，不表示修复了 DM8 引擎的任意后续刷新行为。

事件触发器保留原始事件、调度、WHEN 条件和完整过程体，在 `DECLARE` / `BEGIN` 前加入达梦原生 `DISABLE` 头部子句，从 CREATE 起原子保持停用。TIMER 和 CHECKPOINT 使用同一路径，CHECKPOINT 调度开启时也可迁移；不修改实例参数或其他对象的启停状态。安装后从 `ALL_TRIGGERS` 确认停用状态，确认成功才记为安装完成。整个导入、编译和内容核对期间保持停用。全部核对通过后先恢复物化视图自动刷新策略，再使用最后一个固定连接恢复源事件启停状态，不再执行后续迁移查询或 DDL。恢复自动任务和事件标志着停写导入窗口结束；随后关闭最终会话可能正常触发已恢复的 LOGOUT 逻辑。目标已有的启用事件钩子可能在迁移 DDL 中产生副作用，预检会按其实际事件作用范围阻止执行，包括其他 Schema 所有但针对当前 Schema 的事件钩子。

`ON DATABASE` 保留数据库级作用范围。源端已启用的 LOGIN/LOGOUT 逻辑也可能在建立或关闭源连接时运行；停写安排需要包含这些自动写入路径，现有 Go 接口不能在登录发生前设置会话抑制。不能把 Schema DDL 钩子的测试结果解释为所有数据库事件均无连接副作用。

DBLink 的登录口令只保留在内存中用于原生执行。页面、报告、修复账本与执行错误不保存口令，只展示脱敏定义和冻结源定义的指纹。重启后重试必须能重新连接原源库，读取同一 DBLink 原生定义并核对指纹；定义或凭据变化后拒绝旧计划。目标服务器需要的 DPI/ODBC/OCI 库、DSN 或 MAL 配置仍按外部资源提前准备，不会修改实例配置。

复杂对象和集合支持嵌套对象、数组、NULL、空/非空 BLOB/CLOB，以及高精度负定点数。仓库固定达梦驱动 v1.8.23 的本地副本，仅修复嵌套 LOB 的内容偏移/字符字节长度及负定点数尾数截断，保留上游版权信息；补丁和直接调用驱动编码器/解码器的回归测试见 [third_party/dm/DBGOLD_PATCHES.md](../third_party/dm/DBGOLD_PATCHES.md)。

普通 BFILE 列通过现有驱动原样复制 `目录:文件名` 定位器及 NULL，保留目录和文件名大小写；目录名称不参与 Schema 映射。源端完整冻结并去重这些外部引用，目标预检及最终核验通过原生 `FILEEXISTS(BFILENAME(...))` 确认文件可用和读取权限。目录及外部文件属于提前准备的实例资源，本任务不会创建目录或复制服务器上的外部文件内容；内容校验比较数据库内存储的定位器。目标独有表的原 BFILE 值及外部依赖保持不变。

UDT 内 BFILE 成员的递归外部资源盘点尚未实现，本次 BFILE 验收覆盖普通列；不能把通用对象绑定能力视作该组合的完整资源验收。

当前实测构建的 XMLTYPE 以 TEXT/CLOB 和原生刻度 8 表示，但原生 TABLE DDL 会遗漏该属性。提取过程按目录证据恢复相应列的 XMLTYPE 声明，并保留刻度参与核验；数据沿原始 LOB 通道直接绑定，不用 XML 序列化替代原文，避免改写空白、注释、命名空间和 CDATA。

## 当前会阻止迁移的具体情况

“盘点到”不等于所有厂商扩展均可自动回放。遇到以下情况会明确报错，而不是跳过对象后宣称全量成功：

- 原始 DDL、对象归属、约束状态、源序列/自增边界无法读取或无法安全解析；目录出现尚无回放实现的对象类型。
- 无法确认源目标实例身份、版本或语义参数，或者目标缺少外部依赖、表空间、授权主体及必要权限。
- 无法读取完整物化历史日志、确认承载表及原始分区定义，或由 ROWID 派生的表达式无法安全映射。陈旧 `FAST` / `FORCE` 本身不再是阻止条件；执行时仍须通过旧快照、日志和原生状态的完整核对。
- 生成列作为 INTERVAL 分区键且已有自动分区时，任意用户函数、非线性表达式、UDT、生成列链，以及当前求解候选未覆盖的受限数值域仍没有通用恢复实现。常量及已验证数值仿射表达式已有真实恢复支持；其余分支会保留完整分区清单并报错，仍属于未完成的支持范围。
- DBLink 的原生元数据只返回掩码、绑定占位符或未支持的不可移植口令密文，无法建立可回放的完整定义；重试时源定义或凭据与冻结指纹不符。
- 目标已有、可能影响本任务的启用事件钩子会阻止迁移，需先停用，避免既有业务逻辑影响迁移。新迁移的 TIMER / CHECKPOINT 触发器从创建起保持禁用，不受该限制。
- 单个普通 LOB 超过驱动可安全读取的长度，或 LOB 长度无法核验。
- 驱动没有明确无损编码器的厂商扩展列，或无法完整识别的特殊存储类型。复杂对象/集合值需由驱动返回支持的结构/数组表示；无法无损绑定的元素会报错。普通 BFILE 定位器及本次实测的 XMLTYPE 原生表示已具备迁移实现。
- 数据不是有效 UTF-8、LOB/对象值无法安全读取或绑定，或执行时发现目标定义、状态与计划不符。结构预检不全量扫描每个业务值，因此数据阶段仍可能失败。

定点数通过精确十进制文本绑定，字符值保留 NUL，二进制值按字节复制；LOB 使用驱动 LOB 参数，复杂对象/集合按驱动结构绑定。上述行为仍需在实际 DM8 版本、参数和驱动环境完成真实验收，不能以单元测试代替。

## 验证

本地自动检查：

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate/... ./api/handler
/Users/kay/sdk/go1.25.5/bin/go build ./...
cd third_party/dm
/Users/kay/sdk/go1.25.5/bin/go test ./...
cd ../../frontend
npm test
npm run build
```

完整链路实库测试为 `TestDamengNativeIntegration`。它必须使用两个预建、独立、初始完全为空的专用测试 Schema，不会创建用户或 Schema。可使用同一 DM8 实例上的两个不同 Schema。先通过安全的环境配置提供下列变量，避免将真实凭据写入版本库：

- `DAMENG_NATIVE_SOURCE_DSN`、`DAMENG_NATIVE_SOURCE_SCHEMA`
- `DAMENG_NATIVE_TARGET_DSN`、`DAMENG_NATIVE_TARGET_SCHEMA`
- `DAMENG_NATIVE_ALLOW_DDL=1`

运行：

```sh
/Users/kay/sdk/go1.25.5/bin/go test ./datamigrate -run '^TestDamengNativeIntegration$' -v -count=1
```

测试在这两个 Schema 内创建并清理唯一命名的夹具，包含混合大小写、约束/索引/注释、序列、自增进度、函数/过程/包、触发器、对象类型数据、同义词/授权以及故意陈旧的 COMPLETE/ON DEMAND 物化视图。断言 38 位定点数、二进制/NUL、LOB、触发器复制无副作用、后续写入、冲突关闭零变更、覆盖恢复和目标独有对象保留。

缺少任一变量或 DDL 开关时测试显式跳过；启用后目录权限、夹具语法或迁移失败都会使测试失败。仅看到编译和跳过结果不表示已通过真实 DM8 验收。

其他独立实库夹具：

- 物化视图：`TestDamengNativeMaterializedPipelineIntegration` 与 `TestDamengNativeStaleMaterializedHistoryIntegration` 使用 `DAMENG_NATIVE_SOURCE_DSN/SOURCE_SCHEMA/TARGET_DSN/TARGET_SCHEMA`、`DAMENG_NATIVE_ALLOW_DDL=1`、`DAMENG_NATIVE_ALLOW_MV_DDL=1`；后者包含共享日志且水位不同的旧快照、主键/ROWID、重复行、RANGE/HASH 承载表及响应丢失恢复。底层 `TestDamengNativeMaterializedBaselineIntegration` 使用 `DAMENG_DM_TEST_TARGET_DSN/TARGET_SCHEMA`、`DAMENG_DM_TEST_ALLOW_DDL=1`、`DAMENG_DM_TEST_ALLOW_MV_DDL=1`。
- 分区与恢复：`TestDamengNativeEmptyPartitionRouteHistoryIntegration`、`TestDamengNativeDeferredMaterializedIntegration`、`TestDamengNativeHistoryIndexRecoveryIntegration` 使用同组 SOURCE/TARGET 变量和两个 DDL 开关；分别核对空分区基表路由、延迟构建状态，以及历史恢复完成后索引失败的账本重载补迁和后续实际 FAST。
- 普通 INTERVAL 表：`TestDamengNativeIntervalNonemptyPreparationIntegration` 使用同组 SOURCE/TARGET 变量和 `DAMENG_NATIVE_ALLOW_DDL=1`，核对有数据的表、已删空自动分区、原名称、自增、默认序列及约束。
- INTERVAL 故障补迁：`TestDamengNativeIntervalUnknownPreparationIntegration` 使用同组 SOURCE/TARGET 变量和 `DAMENG_NATIVE_ALLOW_DDL=1`，模拟准备完成后响应丢失及临时属性清理中断，核对目录归并、属性恢复及稀疏分区不被重复创建。
- 约束与覆盖：`TestDamengNativeConstraintStateIntegration`、`TestDamengNativeOwnedNameConflictIntegration` 使用同组 SOURCE/TARGET 变量和 `DAMENG_NATIVE_ALLOW_DDL=1`，分别核对用户索引绑定、NOVALIDATE 历史数据和子对象冲突零写入。
- 标量/UDT：`TestDamengNativeIntegrationExactCopy`、`TestDamengNativeIntegrationNestedUDTExactRoundtrip` 使用 `DAMENG_DM_TEST_TARGET_DSN/TARGET_SCHEMA` 和 `DAMENG_DM_TEST_ALLOW_DDL=1`。
- BFILE/XMLTYPE：`TestDamengNativeIntegrationBFileLocatorAndXMLType` 使用同组 SOURCE/TARGET 变量、`DAMENG_NATIVE_ALLOW_DDL=1` 和 `DAMENG_NATIVE_BFILE_DIRECTORY/BFILE_FILENAME`。两个实例须预先准备同名目录及可读文件；夹具只创建 Schema 内的表和数据，核对缺失资源时目标零写入、定位器与 XML 原值、完整定义和账本重载。
- XMLTYPE 历史快照：`TestDamengNativeXMLTypeMaterializedHistoryIntegration` 使用同组 SOURCE/TARGET 变量和两个 DDL 开关，覆盖普通预建承载表、RANGE 预建承载表及已有自动 INTERVAL 分区的空基表，核对历史恢复暂存、物理分区和后续实际 FORCE。
- 事件：`TestDamengNativeEventLifecycleIntegration` 使用 `DAMENG_NATIVE_EVENT_DSN/EVENT_SCHEMA`；`TestDamengNativeEventMigrationIntegration` 使用 `DAMENG_NATIVE_EVENT_SOURCE_DSN/EVENT_SOURCE_SCHEMA/EVENT_TARGET_DSN/EVENT_TARGET_SCHEMA`，均需 `DAMENG_NATIVE_ALLOW_DDL=1`。`TestDamengNativeBackgroundEventMigrationIntegration` 使用同组跨实例连接变量，专用目标实例须预先开启 `ENABLE_CKPT_EVENT_TRIG=1`；测试自身不改实例参数，配置 `DAMENG_NATIVE_BACKGROUND_WAIT_TIMER=1` 时还等待验证真实 TIMER 调度。
- DBLink：`TestDamengNativeDBLinkIntegration` 使用 `DAMENG_NATIVE_LINK_SOURCE_DSN/SOURCE_SCHEMA/TARGET_DSN/TARGET_SCHEMA/REMOTE_CONNECT/REMOTE_USERNAME/REMOTE_PASSWORD` 及 `DAMENG_NATIVE_ALLOW_DDL=1`。远端必须是从源、目标服务器均可访问的第三个专用 DM 实例，测试只读取远端目录，不创建远端对象。

2026-10-22 已在从本机现有镜像启动的独立临时 DM8 容器中实测：`1-4-6-2024.09.19-243448-20119-ENT`、字典版本 `0x7000c`、`COMPATIBLE_MODE=2`、UTF-8、大小写敏感。上述普通整 Schema 夹具通过同实例异名 Schema 和跨实例两种运行；FAST/FORCE × 主键/ROWID 四种组合通过基线、完整内容核对、用户索引保留与后续实际 FAST 刷新；嵌套 UDT/LOB、标量 NULL/空 LOB、事件 DDL 钩子完整迁移和三实例 DBLink 查询通过。实测同时修复了兼容视图遗漏 MV/列注释、原生授权缺少分隔符/大小写引号、原生包编译语法及驱动集合描述符等问题。

本次后台事件补充验收已在两套独立临时 DM8 实例通过：TIMER / CHECKPOINT 各包含原启用与原禁用对象，目标 CHECKPOINT 调度保持开启，完整 Schema 数据和定义核对后恢复各自状态；切换后的真实 CHECKPOINT 异步事件和 TIMER 原调度均执行原业务体，禁用对象保持不触发。旧冻结账本中不带 DISABLE 的未知 CREATE 也先按目录核对再确认完成，已成功 CREATE 不重放。原有 Schema DDL 事件生命周期与完整迁移回归同样通过。

本次历史恢复实库验收包含两个共享日志但刷新水位不同的陈旧快照、FAST/FORCE × 主键/ROWID、无主键重复行、RANGE/HASH 承载、空基表、匿名 CHECK、NOT NULL、值相等但状态陈旧、原 NEEDS_FULL_REFRESH、无日志 FORCE 和 UNION ALL。校验旧快照内容、目标定义/分区/状态、重载冻结账本及真实后续 FAST/FORCE；HASH 另覆盖 START、FINISH 和三步重命名中断，历史组覆盖全部交换完成后的成功响应丢失。空表恢复还独立实测 UDT/域、默认值序列不消费、外键 NOVALIDATE 及中断清理。

2026-10-23 在同构建的两套独立临时 DM8 实例追加通过：负 RANGE、INT 最小值边界、负 INTERVAL、数值/按月日期的已有空自动分区、包含上界且步长为 1 的 INTERVAL、DATE 边界、RANGE→LIST、三级 LIST 和生成 RANGE 分区键的完整迁移、内容校验、冻结账本重载及后续 FORCE；独立/共享日志中的 BUILD DEFERRED；用户索引绑定主键、独立 DESC 聚集索引、命名/匿名检查与唯一约束 NOVALIDATE、匿名禁用约束及重复约束数量；GLOBAL 索引故障后的补迁保持旧快照与水位。分区派生子索引按原生物理父链归属，保留根用户索引。实际 Schema 所有者与独立 MLOG 权限也完成核对。普通全 Schema、15 种陈旧快照组合、TIMER/CHECKPOINT 真实触发、标量及嵌套 UDT 回归通过。以上均仅使用现有 Go 驱动。

非空 INTERVAL 表追加通过完整迁移、内容校验和账本重载最终核验：包含原名的已删空自动分区、GLOBAL 用户索引绑定主键、出入向外键、NOVALIDATE、自增及 `DEFAULT ON NULL` 序列，迁移后真实写入的续号与默认值均符合源状态。分区活动序列的独立实测覆盖正负步长、显式极值、全部删除和多次重建留下的历史序列。约束夹具还核对分区派生约束数量、重复匿名约束缺失后的检测与补迁。

最终分区实库夹具共通过 16 类路由及陈旧 FORCE 场景，其中 6 类使用生成分区键（单列倍数、多列组合、需组合求解的系数、定点数、负系数和仅字符串可写列的常量生成键）；4 类未知回执/清理中断同时覆盖普通键和生成键，核对物理分区 ID、纯读归并及恢复后真实业务写入。默认值独立实库夹具通过 CHECK/UNIQUE 第二行失败后的整批零提交、原默认表达式恢复及后续真实序列消费。

BFILE/XMLTYPE 的整 Schema 实库夹具通过大小 XML、NULL、重复定位器、混合大小写目录、NUL 文本、完整内容校验及账本 JSON 重载后的最终核验；源文件缺失时目标本次表零创建，资源可用后正常迁移，目标独有的缺失外部文件引用保留。XMLTYPE 同时核对原生刻度，不能以 XML 文本相等掩盖属性丢失。

XMLTYPE 历史快照的三类组合均通过完整迁移、内容校验、账本重载和后续实际 FORCE：普通预建承载表、明确绑定且包含两个 RANGE 分区的预建承载表、已有自动 INTERVAL 分区的空基表。源快照原始 XML 与目标逐值相同，历史恢复暂存及继承根表列属性的物理分区均保留 XMLTYPE 原生刻度 8；原有 16 类非 XML 分区场景随后全部回归通过。

这仍不是“所有对象无例外”的完整验收：所有表达式索引/约束变体、所有数据库事件、其他 DM8 构建/兼容模式以及生产规模故障恢复尚需进一步实测。临时夹具通过不会撤销上面列出的具体边界。

夹具语法参考达梦官方 SQL 手册：[包](https://eco.dameng.com/document/dm/zh-cn/pm/package.html)、[自定义类型](https://eco.dameng.com/document/dm/zh-cn/pm/custom-type)、[物化视图](https://eco.dameng.com/document/dm/zh-cn/pm/materialized-view.html)、[DBMS_REDEFINITION](https://eco.dameng.com/document/dm/zh-cn/pm/dbms_redefinition-package.html)、[原生系统字典](https://eco.dameng.com/document/dm/zh-cn/pm/dm8-admin-manual-appendix1.html)。
