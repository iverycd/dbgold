package source

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const mysqlTriggerCharacterColumnsSQL = `SELECT TABLE_SCHEMA,TABLE_NAME,COLUMN_NAME,CHARACTER_SET_NAME,COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND CHARACTER_SET_NAME IS NOT NULL ORDER BY TABLE_NAME,ORDINAL_POSITION`
const mysqlTriggerNumericColumnsSQL = `SELECT TABLE_SCHEMA,TABLE_NAME,COLUMN_NAME,COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND DATA_TYPE IN ('tinyint','smallint','mediumint','int','integer','bigint','decimal','numeric') ORDER BY TABLE_NAME,ORDINAL_POSITION`
const mysqlTriggerCompareSQL = `SELECT CONVERT(CAST(? AS BINARY) USING utf8mb4) COLLATE utf8mb4_general_ci = CONVERT(CAST(? AS BINARY) USING utf8mb4) COLLATE utf8mb4_general_ci`
const triggerWeightChunkSize = 4096

func mysqlTriggerWeightSQL(characters int) string {
	return fmt.Sprintf(`SELECT HEX(WEIGHT_STRING(CONVERT(CAST(? AS BINARY) USING utf8mb4) COLLATE utf8mb4_general_ci AS CHAR(%d)))`, characters)
}

func supportedTriggerMySQLServer(version, comment string) bool {
	if !supportedTriggerMySQLVersion(version + " " + comment) {
		return false
	}
	// Source builds use this default comment without the MySQL product name.
	// Keep it an exact label rather than accepting arbitrary vendor comments.
	comment = strings.TrimSpace(comment)
	return strings.Contains(comment, "MySQL") || strings.EqualFold(comment, "Source distribution")
}

func (r *MySQLReader) GetTriggerCharacterMetadata(ctx context.Context) (*TriggerCharacterMetadata, error) {
	var version, comment string
	if err := r.queryRowContext(ctx, `SELECT VERSION(),@@version_comment`).Scan(&version, &comment); err != nil {
		return nil, fmt.Errorf("读取 MySQL 字符排序版本: %w", err)
	}
	if !supportedTriggerMySQLServer(version, comment) {
		return nil, fmt.Errorf("严格字符比较仅验证官方 MySQL 5.7、8.0、8.4，当前 %s %s", version, comment)
	}
	meta := &TriggerCharacterMetadata{ServerVersion: version + " " + comment}
	rows, err := r.queryContext(ctx, mysqlTriggerCharacterColumnsSQL, r.dbName)
	if err != nil {
		return nil, fmt.Errorf("读取源字符列排序规则: %w", err)
	}
	needGeneral := false
	for rows.Next() {
		var col TriggerColumnCharacter
		if err = rows.Scan(&col.Schema, &col.Table, &col.Name, &col.Charset, &col.Collation); err != nil {
			rows.Close()
			return nil, err
		}
		meta.Columns = append(meta.Columns, col)
		if col.Charset == "utf8mb4" && col.Collation == "utf8mb4_general_ci" {
			needGeneral = true
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	rows, err = r.queryContext(ctx, mysqlTriggerNumericColumnsSQL, r.dbName)
	if err != nil {
		return nil, fmt.Errorf("读取源数值列格式: %w", err)
	}
	for rows.Next() {
		var col TriggerNumericColumn
		if err = rows.Scan(&col.Schema, &col.Table, &col.Name, &col.ColumnType); err != nil {
			rows.Close()
			return nil, err
		}
		meta.NumericColumns = append(meta.NumericColumns, col)
	}
	err = rows.Err()
	closeErr = rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	sort.Slice(meta.Columns, func(i, j int) bool {
		a, b := meta.Columns[i], meta.Columns[j]
		return a.Schema+"\x00"+a.Table+"\x00"+a.Name < b.Schema+"\x00"+b.Table+"\x00"+b.Name
	})
	sort.Slice(meta.NumericColumns, func(i, j int) bool {
		a, b := meta.NumericColumns[i], meta.NumericColumns[j]
		return a.Schema+"\x00"+a.Table+"\x00"+a.Name < b.Schema+"\x00"+b.Table+"\x00"+b.Name
	})
	if needGeneral {
		profile, err := r.readTriggerGeneralCIProfile(ctx)
		if err != nil {
			return nil, err
		}
		meta.Profiles = append(meta.Profiles, profile)
	}
	if err = ValidateTriggerCharacterMetadata(meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func triggerBMPCodepoints() []rune {
	codepoints := make([]rune, 0, 63487)
	for cp := rune(1); cp <= 0xffff; cp++ {
		if cp >= 0xd800 && cp <= 0xdfff {
			continue
		}
		codepoints = append(codepoints, cp)
	}
	return codepoints
}

func (r *MySQLReader) readTriggerGeneralCIProfile(ctx context.Context) (TriggerCollationProfile, error) {
	p := TriggerCollationProfile{Charset: "utf8mb4", Collation: "utf8mb4_general_ci", PadAttribute: "PAD SPACE", SupplementaryWeight: 0xfffd}
	codepoints := triggerBMPCodepoints()
	weights := make(map[rune]uint16, len(codepoints))
	var from, to strings.Builder
	for start := 0; start < len(codepoints); start += triggerWeightChunkSize {
		end := min(start+triggerWeightChunkSize, len(codepoints))
		chars := codepoints[start:end]
		var raw sql.NullString
		if err := r.queryRowContext(ctx, mysqlTriggerWeightSQL(len(chars)), []byte(string(chars))).Scan(&raw); err != nil {
			return p, fmt.Errorf("读取 utf8mb4_general_ci BMP 权重: %w", err)
		}
		data, err := hex.DecodeString(raw.String)
		if !raw.Valid || err != nil || len(data) != len(chars)*2 {
			return p, fmt.Errorf("utf8mb4_general_ci BMP 权重长度不符合一字符两字节，停止严格转换")
		}
		for i, cp := range chars {
			weight := binary.BigEndian.Uint16(data[i*2 : i*2+2])
			if weight == 0 || weight >= 0xd800 && weight <= 0xdfff {
				return p, fmt.Errorf("字符 U+%04X 的排序权重无法无损映射到 PG 文本", cp)
			}
			if cp == 0x20 && weight != 0x20 || cp != 0x20 && weight == 0x20 || cp == 0xfffd && weight != 0xfffd {
				return p, fmt.Errorf("源 general_ci 的 SPACE/FFFD 权重不符合已验证规则")
			}
			weights[cp] = weight
			if weight != uint16(cp) {
				from.WriteRune(cp)
				to.WriteRune(rune(weight))
			}
		}
	}
	for _, value := range []string{"\U00010000", "\U0001f600", "\U00020000", "\U0010ffff"} {
		var raw sql.NullString
		if err := r.queryRowContext(ctx, mysqlTriggerWeightSQL(1), []byte(value)).Scan(&raw); err != nil {
			return p, err
		}
		if !raw.Valid || !strings.EqualFold(raw.String, "FFFD") {
			return p, fmt.Errorf("源 general_ci 补充平面字符权重不是 FFFD，无法严格转换")
		}
	}
	// Confirm that observed keys agree with real comparison semantics, including
	// PAD SPACE. Single-character weight collection alone does not test padding.
	for _, pair := range [][2]string{{"a", "A"}, {"á", "A"}, {"ß", "s"}, {"a", "a  "}, {"a", "a\u00a0"}, {"e\u0301", "é"}, {"\U0001f600", "\U00020000"}, {"", " "}} {
		var actual bool
		if err := r.queryRowContext(ctx, mysqlTriggerCompareSQL, []byte(pair[0]), []byte(pair[1])).Scan(&actual); err != nil {
			return p, err
		}
		if actual != triggerWeightEqual(pair[0], pair[1], weights) {
			return p, fmt.Errorf("源 general_ci 实际比较与采集权重不一致，无法严格转换")
		}
	}
	p.From, p.To = from.String(), to.String()
	p.Digest = TriggerCollationDigest(p)
	return p, nil
}

func triggerWeightEqual(a, b string, weights map[rune]uint16) bool {
	key := func(s string) string {
		var out strings.Builder
		for _, cp := range s {
			weight := uint16(0xfffd)
			if cp <= 0xffff {
				weight = weights[cp]
			}
			out.WriteRune(rune(weight))
		}
		return strings.TrimRight(out.String(), " ")
	}
	return key(a) == key(b)
}
