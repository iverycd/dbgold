package valueconv

import "encoding/hex"

// DaMengValueConverter 把 Reader 输出的中立值落地为达梦(dm 驱动)能接受的形态。
//
// 与 PG 不同:达梦 dm 驱动通过参数化 INSERT 接收值,能直接接受 time.Time,
// 无需把日期格式化成字符串;BLOB/BINARY 保持 []byte。
// 仅 MySQL 的 BIT/GEOMETRY(中立值为 []byte)需要转换为达梦列能接受的形态。
type DaMengValueConverter struct{}

func NewDaMeng() *DaMengValueConverter { return &DaMengValueConverter{} }

func (c *DaMengValueConverter) Convert(val interface{}, srcType, dbTypeName string) interface{} {
	if val == nil {
		return nil
	}
	if srcType == "mysql" {
		if b, ok := val.([]byte); ok {
			switch dbTypeName {
			case "BIT":
				// 目标为 NUMBER(1):把位串字节转十进制整数
				return bytesToInt(b)
			case "GEOMETRY":
				// 几何类型映射为文本(CLOB),用十六进制串表示
				if len(b) >= 4 {
					return hex.EncodeToString(b)[8:]
				}
				return hex.EncodeToString(b)
			}
		}
	}
	if srcType == "postgres" {
		// pg boolean 经 lib/pq 返回 Go bool，目标 NUMBER(1) 需要落地为 0/1
		if b, ok := val.(bool); ok {
			if b {
				return 1
			}
			return 0
		}
	}
	// 其余类型(time.Time、字符串、[]byte BLOB、数值)直接交给 dm 驱动
	return val
}

// bytesToInt 把大端字节序列转为整数(用于 MySQL BIT → 达梦 NUMBER)。
func bytesToInt(b []byte) int64 {
	var n int64
	for _, x := range b {
		n = n<<8 | int64(x)
	}
	return n
}
