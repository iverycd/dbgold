// Package valueconv 定义「目标值转换」抽象。
//
// 背景:各源 Reader 的 ReadPage 历史上把数据值转成「PostgreSQL 友好」形态
// (日期格式化成字符串、MySQL bit → "0"/"1" 文本、SqlServer UUID/MONEY/XML → string),
// 因为 pq 的 COPY 协议不接受 time.Time、bit(1) 只认文本。这使得值转换绑死了 PG。
//
// 重构后:Reader 输出「中立值」(time.Time 原样、bit/geometry 保持 []byte 等),
// 由目标侧的 ValueConverter 按列类型名把中立值落地成各目标库驱动能接受的形态。
// 这样新增非-PG 目标库(如达梦)时,值落地策略独立可控。
package valueconv

// ValueConverter 把 Reader 输出的中立值落地成目标库驱动能接受的形态。
//
// 参数:
//   - val:        中立值(可能为 nil)
//   - srcType:    源库类型,如 "mysql" / "sqlserver"(同一 dbTypeName 在不同源库语义不同)
//   - dbTypeName: 该列的 DatabaseTypeName(大写),如 "BIT" / "DATETIME" / "UNIQUEIDENTIFIER"
type ValueConverter interface {
	Convert(val interface{}, srcType, dbTypeName string) interface{}
}
