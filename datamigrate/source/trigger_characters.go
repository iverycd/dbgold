package source

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Trigger-specific character metadata stays separate from ColumnInfo so old
// table/data-recovery snapshots retain their original hashes.
type TriggerCharacterMetadata struct {
	ServerVersion  string
	Columns        []TriggerColumnCharacter
	NumericColumns []TriggerNumericColumn
	Profiles       []TriggerCollationProfile
}
type TriggerColumnCharacter struct{ Schema, Table, Name, Charset, Collation string }
type TriggerNumericColumn struct{ Schema, Table, Name, ColumnType string }
type TriggerCollationProfile struct {
	Charset, Collation, PadAttribute, From, To, Digest string
	SupplementaryWeight                                uint16
}
type TriggerCharacterReader interface {
	GetTriggerCharacterMetadata(context.Context) (*TriggerCharacterMetadata, error)
}

// Digest binds the canonical sparse source-codepoint-to-weight mapping. NUL
// separators are unambiguous because profile text is validated to exclude NUL.
func TriggerCollationDigest(p TriggerCollationProfile) string {
	h := sha256.New()
	h.Write([]byte("dbgold:mysql-trigger:general-ci:v2\x00"))
	for _, s := range []string{p.Charset, p.Collation, p.PadAttribute, p.From, p.To} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], p.SupplementaryWeight)
	h.Write(b[:])
	return hex.EncodeToString(h.Sum(nil))
}

var triggerMySQLVersion = regexp.MustCompile(`^(5\.7|8\.0|8\.4)\.[0-9]+(?:-log)?$`)
var triggerNumericColumnType = regexp.MustCompile(`(?i)^(tinyint|smallint|mediumint|int|integer|bigint|decimal|numeric)(\([0-9]+(?:,[0-9]+)?\))?(?: unsigned)?(?: zerofill)?$`)

func supportedTriggerMySQLVersion(version string) bool {
	parts := strings.Fields(version)
	if len(parts) == 0 || !triggerMySQLVersion.MatchString(parts[0]) {
		return false
	}
	lower := strings.ToLower(version)
	for _, vendor := range []string{"mariadb", "percona", "tidb", "oceanbase", "greatsql", "polardb"} {
		if strings.Contains(lower, vendor) {
			return false
		}
	}
	return true
}

func ValidateTriggerCharacterMetadata(meta *TriggerCharacterMetadata) error {
	if meta == nil {
		return fmt.Errorf("缺少源字符排序元数据")
	}
	if !supportedTriggerMySQLVersion(meta.ServerVersion) {
		return fmt.Errorf("字符比较仅验证官方 MySQL 5.7、8.0、8.4，当前 %s", meta.ServerVersion)
	}
	columns := map[string]bool{}
	for _, col := range meta.Columns {
		for _, s := range []string{col.Schema, col.Table, col.Name, col.Charset, col.Collation} {
			if s == "" || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
				return fmt.Errorf("源字符列元数据不完整")
			}
		}
		key := col.Schema + "\x00" + col.Table + "\x00" + col.Name
		if columns[key] {
			return fmt.Errorf("源字符列元数据重复")
		}
		columns[key] = true
	}
	profiles := map[string]bool{}
	for _, col := range meta.NumericColumns {
		for _, s := range []string{col.Schema, col.Table, col.Name, col.ColumnType} {
			if s == "" || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
				return fmt.Errorf("源数值列格式元数据不完整")
			}
		}
		key := col.Schema + "\x00" + col.Table + "\x00" + col.Name
		if columns[key] {
			return fmt.Errorf("源数值列元数据重复或与字符列冲突")
		}
		columns[key] = true
		if !triggerNumericColumnType.MatchString(col.ColumnType) {
			return fmt.Errorf("不支持的源数值列格式 %s", col.ColumnType)
		}
	}
	for _, p := range meta.Profiles {
		if p.Charset != "utf8mb4" || p.Collation != "utf8mb4_general_ci" || p.PadAttribute != "PAD SPACE" || p.SupplementaryWeight != 0xfffd {
			return fmt.Errorf("不支持的源字符排序规则配置")
		}
		if profiles[p.Collation] {
			return fmt.Errorf("源字符排序 profile 重复")
		}
		profiles[p.Collation] = true
		if !utf8.ValidString(p.From) || !utf8.ValidString(p.To) {
			return fmt.Errorf("排序权重映射不是合法 UTF8")
		}
		from, to := []rune(p.From), []rune(p.To)
		if len(from) == 0 || len(from) != len(to) {
			return fmt.Errorf("源排序权重映射为空或不完整")
		}
		for i, cp := range from {
			if cp <= 0 || cp > 0xffff || cp >= 0xd800 && cp <= 0xdfff || i > 0 && cp <= from[i-1] {
				return fmt.Errorf("源排序权重键无效或顺序不规范")
			}
			weight := to[i]
			if weight <= 0 || weight > 0xffff || weight >= 0xd800 && weight <= 0xdfff || weight == cp {
				return fmt.Errorf("源排序权重值无效或非稀疏映射")
			}
			if cp == 0x20 || weight == 0x20 || cp == 0xfffd {
				return fmt.Errorf("源排序规则的 SPACE 或 FFFD 权重不兼容")
			}
		}
		if p.Digest != TriggerCollationDigest(p) {
			return fmt.Errorf("源字符排序权重摘要不匹配")
		}
	}
	return nil
}
