package source

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func characterReaderFixture(t *testing.T) (*MySQLReader, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &MySQLReader{db: db, dbName: "SourceDB"}, m
}
func mockGeneralWeight(cp rune) uint16 {
	if cp > 0xffff {
		return 0xfffd
	}
	if cp >= 'a' && cp <= 'z' {
		return uint16(cp - 'a' + 'A')
	}
	if cp == 'á' {
		return 'A'
	}
	if cp == 'ß' {
		return 'S'
	}
	return uint16(cp)
}
func syntheticWeights(chars []rune) string {
	b := make([]byte, len(chars)*2)
	for i, cp := range chars {
		binary.BigEndian.PutUint16(b[i*2:], mockGeneralWeight(cp))
	}
	return hex.EncodeToString(b)
}
func expectGeneralWeightRead(m sqlmock.Sqlmock) {
	chars := triggerBMPCodepoints()
	weights := map[rune]uint16{}
	for _, cp := range chars {
		weights[cp] = mockGeneralWeight(cp)
	}
	for start := 0; start < len(chars); start += triggerWeightChunkSize {
		end := min(start+triggerWeightChunkSize, len(chars))
		chunk := chars[start:end]
		m.ExpectQuery(mysqlTriggerWeightSQL(len(chunk))).WithArgs([]byte(string(chunk))).WillReturnRows(sqlmock.NewRows([]string{"weights"}).AddRow(syntheticWeights(chunk)))
	}
	for _, value := range []string{"\U00010000", "\U0001f600", "\U00020000", "\U0010ffff"} {
		m.ExpectQuery(mysqlTriggerWeightSQL(1)).WithArgs([]byte(value)).WillReturnRows(sqlmock.NewRows([]string{"weight"}).AddRow("FFFD"))
	}
	for _, pair := range [][2]string{{"a", "A"}, {"á", "A"}, {"ß", "s"}, {"a", "a  "}, {"a", "a\u00a0"}, {"e\u0301", "é"}, {"\U0001f600", "\U00020000"}, {"", " "}} {
		m.ExpectQuery(mysqlTriggerCompareSQL).WithArgs([]byte(pair[0]), []byte(pair[1])).WillReturnRows(sqlmock.NewRows([]string{"equal"}).AddRow(triggerWeightEqual(pair[0], pair[1], weights)))
	}
}

func TestMySQLTriggerCharactersCaptureFullBMPAndOriginalNames(t *testing.T) {
	r, m := characterReaderFixture(t)
	m.ExpectQuery(`SELECT VERSION(),@@version_comment`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow("8.4.11", "MySQL Community Server - GPL"))
	m.ExpectQuery(mysqlTriggerCharacterColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "charset", "collation"}).AddRow("SourceDB", "MiXeD", "UsbKey", "utf8mb4", "utf8mb4_general_ci").AddRow("SourceDB", "MiXeD", "Other", "latin1", "latin1_swedish_ci"))
	m.ExpectQuery(mysqlTriggerNumericColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "type"}).AddRow("SourceDB", "MiXeD", "IntCode", "int(6) unsigned zerofill").AddRow("SourceDB", "MiXeD", "MoneyCode", "decimal(8,2) unsigned zerofill"))
	expectGeneralWeightRead(m)
	meta, err := r.GetTriggerCharacterMetadata(context.Background())
	require.NoError(t, err)
	require.Equal(t, "8.4.11 MySQL Community Server - GPL", meta.ServerVersion)
	require.Contains(t, meta.Columns, TriggerColumnCharacter{Schema: "SourceDB", Table: "MiXeD", Name: "UsbKey", Charset: "utf8mb4", Collation: "utf8mb4_general_ci"})
	require.Equal(t, []TriggerNumericColumn{{Schema: "SourceDB", Table: "MiXeD", Name: "IntCode", ColumnType: "int(6) unsigned zerofill"}, {Schema: "SourceDB", Table: "MiXeD", Name: "MoneyCode", ColumnType: "decimal(8,2) unsigned zerofill"}}, meta.NumericColumns)
	require.Len(t, meta.Profiles, 1)
	p := meta.Profiles[0]
	require.Equal(t, "abcdefghijklmnopqrstuvwxyzßá", p.From)
	require.Equal(t, "ABCDEFGHIJKLMNOPQRSTUVWXYZSA", p.To)
	from, to := []rune(p.From), []rune(p.To)
	for i, cp := range from {
		require.Equal(t, rune(mockGeneralWeight(cp)), to[i])
	}
	require.Equal(t, TriggerCollationDigest(p), p.Digest)
	require.NoError(t, ValidateTriggerCharacterMetadata(meta))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMySQLTriggerCharacterMetadataRejectsUnknownServer(t *testing.T) {
	for _, version := range []string{"8.1.0", "9.0.0", "5.7.44-MariaDB", "8.0.36-Percona"} {
		t.Run(version, func(t *testing.T) {
			r, m := characterReaderFixture(t)
			m.ExpectQuery(`SELECT VERSION(),@@version_comment`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow(version, "MySQL Community Server - GPL"))
			_, err := r.GetTriggerCharacterMetadata(context.Background())
			require.ErrorContains(t, err, "官方 MySQL")
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
	r, m := characterReaderFixture(t)
	m.ExpectQuery(`SELECT VERSION(),@@version_comment`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow("8.0.36", "Percona Server"))
	_, err := r.GetTriggerCharacterMetadata(context.Background())
	require.Error(t, err)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMySQLTriggerServerIdentityAllowsOnlyKnownBuildLabels(t *testing.T) {
	for _, tc := range []struct {
		version, comment string
		want             bool
	}{
		{"8.0.42", "Source distribution", true},
		{"8.0.42", " source DISTRIBUTION ", true},
		{"5.7.44-log", "MySQL Community Server (GPL)", true},
		{"8.4.11", "MySQL Community Server - GPL", true},
		{"8.0.42", "Unknown Server", false},
		{"8.0.42", "Source distribution customized", false},
		{"8.1.0", "Source distribution", false},
		{"8.0.42-MariaDB", "Source distribution", false},
		{"8.0.42", "MySQL Percona Server", false},
		{"8.0.42", "MySQL MariaDB", false},
		{"8.0.42", "MySQL TiDB", false},
		{"8.0.42", "MySQL OceanBase", false},
		{"8.0.42", "MySQL GreatSQL", false},
		{"8.0.42", "MySQL PolarDB", false},
	} {
		t.Run(tc.version+"/"+tc.comment, func(t *testing.T) {
			require.Equal(t, tc.want, supportedTriggerMySQLServer(tc.version, tc.comment))
		})
	}
}

func TestMySQLTriggerSourceDistributionStillValidatesCharacterWeights(t *testing.T) {
	r, m := characterReaderFixture(t)
	m.ExpectQuery(`SELECT VERSION(),@@version_comment`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow("8.0.42", "Source distribution"))
	m.ExpectQuery(mysqlTriggerCharacterColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "charset", "collation"}).AddRow("SourceDB", "MiXeD", "Note", "utf8mb4", "utf8mb4_general_ci"))
	m.ExpectQuery(mysqlTriggerNumericColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "type"}).AddRow("SourceDB", "MiXeD", "CurrentValue", "int"))
	expectGeneralWeightRead(m)
	meta, err := r.GetTriggerCharacterMetadata(context.Background())
	require.NoError(t, err)
	require.Equal(t, "8.0.42 Source distribution", meta.ServerVersion)
	require.Len(t, meta.Profiles, 1)
	require.Equal(t, "MiXeD", meta.Columns[0].Table)
	require.Equal(t, "CurrentValue", meta.NumericColumns[0].Name)
	require.NoError(t, ValidateTriggerCharacterMetadata(meta))
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMySQLTriggerCharacterMetadataDoesNotReadUnneededProfile(t *testing.T) {
	r, m := characterReaderFixture(t)
	m.ExpectQuery(`SELECT VERSION(),@@version_comment`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow("5.7.20-log", "MySQL Community Server (GPL)"))
	m.ExpectQuery(mysqlTriggerCharacterColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "charset", "collation"}).AddRow("SourceDB", "T", "Name", "utf8mb4", "utf8mb4_unicode_ci"))
	m.ExpectQuery(mysqlTriggerNumericColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "type"}))
	meta, err := r.GetTriggerCharacterMetadata(context.Background())
	require.NoError(t, err)
	require.Empty(t, meta.Profiles)
	require.NoError(t, m.ExpectationsWereMet())
}

func TestMySQLTriggerWeightsFailClosed(t *testing.T) {
	for _, mode := range []string{"packet", "null", "short", "invalid_hex", "nul_weight", "surrogate_weight", "space"} {
		t.Run(mode, func(t *testing.T) {
			r, m := characterReaderFixture(t)
			chars := triggerBMPCodepoints()[:triggerWeightChunkSize]
			q := m.ExpectQuery(mysqlTriggerWeightSQL(len(chars))).WithArgs([]byte(string(chars)))
			if mode == "packet" {
				q.WillReturnError(errors.New("max_allowed_packet exceeded"))
			} else {
				var raw any = syntheticWeights(chars)
				switch mode {
				case "null":
					raw = nil
				case "short":
					raw = "0041"
				case "invalid_hex":
					raw = "zz"
				case "nul_weight":
					raw = "0000" + syntheticWeights(chars)[4:]
				case "surrogate_weight":
					raw = "d800" + syntheticWeights(chars)[4:]
				case "space":
					raw = "0020" + syntheticWeights(chars)[4:]
				}
				q.WillReturnRows(sqlmock.NewRows([]string{"weight"}).AddRow(raw))
			}
			_, err := r.readTriggerGeneralCIProfile(context.Background())
			require.Error(t, err)
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestFrozenTriggerCharacterMetadataValidation(t *testing.T) {
	p := TriggerCollationProfile{Charset: "utf8mb4", Collation: "utf8mb4_general_ci", PadAttribute: "PAD SPACE", From: "a", To: "A", SupplementaryWeight: 0xfffd}
	p.Digest = TriggerCollationDigest(p)
	base := TriggerCharacterMetadata{ServerVersion: "8.4.11 MySQL Community Server - GPL", Columns: []TriggerColumnCharacter{{Schema: "S", Table: "T", Name: "Key", Charset: "utf8mb4", Collation: "utf8mb4_general_ci"}}, Profiles: []TriggerCollationProfile{p}}
	require.NoError(t, ValidateTriggerCharacterMetadata(&base))
	for _, mode := range []string{"digest", "unsorted", "different_length", "identity", "space_from", "space_weight", "fffd", "nonbmp", "invalid_utf8", "version", "duplicate_column"} {
		t.Run(mode, func(t *testing.T) {
			meta := base
			meta.Columns = append([]TriggerColumnCharacter(nil), base.Columns...)
			meta.Profiles = append([]TriggerCollationProfile(nil), base.Profiles...)
			p := &meta.Profiles[0]
			switch mode {
			case "digest":
				p.Digest = ""
			case "unsorted":
				p.From = "ba"
				p.To = "BA"
			case "different_length":
				p.To = "AB"
			case "identity":
				p.To = "a"
			case "space_from":
				p.From = " "
			case "space_weight":
				p.To = " "
			case "fffd":
				p.From = "\ufffd"
			case "nonbmp":
				p.To = "\U0001f600"
			case "invalid_utf8":
				p.From = string([]byte{0xed, 0xa0, 0x80})
			case "version":
				meta.ServerVersion = "8.2.0"
			case "duplicate_column":
				meta.Columns = append(meta.Columns, meta.Columns[0])
			}
			if mode != "digest" {
				p.Digest = TriggerCollationDigest(*p)
			}
			require.Error(t, ValidateTriggerCharacterMetadata(&meta))
		})
	}
}

func TestGeneralCIKeyNullFreePadEquality(t *testing.T) {
	weights := map[rune]uint16{}
	for _, cp := range triggerBMPCodepoints() {
		weights[cp] = mockGeneralWeight(cp)
	}
	require.True(t, triggerWeightEqual("a ", "A", weights))
	require.True(t, triggerWeightEqual("\U0001f600", "\ufffd", weights))
	require.False(t, triggerWeightEqual("e\u0301", "é", weights))
	require.False(t, strings.ContainsRune(string(triggerBMPCodepoints()), 0))
}

func TestMySQLTriggerNumericMetadataWithoutCharacterProfile(t *testing.T) {
	r, m := characterReaderFixture(t)
	m.ExpectQuery(`SELECT VERSION(),@@version_comment`).WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow("8.0.36", "MySQL Community Server - GPL"))
	m.ExpectQuery(mysqlTriggerCharacterColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"s", "t", "n", "charset", "collation"}))
	m.ExpectQuery(mysqlTriggerNumericColumnsSQL).WithArgs("SourceDB").WillReturnRows(sqlmock.NewRows([]string{"schema", "table", "column", "type"}).AddRow("SourceDB", "MixedNumbers", "IntID", "int(11)").AddRow("SourceDB", "MixedNumbers", "DecimalValue", "decimal(8,2)"))
	meta, err := r.GetTriggerCharacterMetadata(context.Background())
	require.NoError(t, err)
	require.Empty(t, meta.Profiles)
	require.Len(t, meta.NumericColumns, 2)
	require.Equal(t, "DecimalValue", meta.NumericColumns[0].Name)
	require.Equal(t, "decimal(8,2)", meta.NumericColumns[0].ColumnType)
	require.NoError(t, m.ExpectationsWereMet())
	for _, mode := range []string{"missing_type", "duplicate", "char_conflict", "unexpected_type"} {
		t.Run(mode, func(t *testing.T) {
			candidate := *meta
			candidate.NumericColumns = append([]TriggerNumericColumn(nil), meta.NumericColumns...)
			switch mode {
			case "missing_type":
				candidate.NumericColumns[0].ColumnType = ""
			case "duplicate":
				candidate.NumericColumns = append(candidate.NumericColumns, candidate.NumericColumns[0])
			case "char_conflict":
				n := candidate.NumericColumns[0]
				candidate.Columns = []TriggerColumnCharacter{{Schema: n.Schema, Table: n.Table, Name: n.Name, Charset: "utf8mb4", Collation: "utf8mb4_general_ci"}}
			case "unexpected_type":
				candidate.NumericColumns[0].ColumnType = "varchar(100)"
			}
			require.Error(t, ValidateTriggerCharacterMetadata(&candidate))
		})
	}
}

func TestMySQLTriggerGeneralCIProfileIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_MYSQL_DSN is not set")
	}
	r, err := NewMySQL(dsn, "", ConnPoolConfig{})
	require.NoError(t, err)
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := r.readTriggerGeneralCIProfile(ctx)
	require.NoError(t, err)
	var version, comment string
	require.NoError(t, r.queryRowContext(ctx, `SELECT VERSION(),@@version_comment`).Scan(&version, &comment))
	require.NoError(t, ValidateTriggerCharacterMetadata(&TriggerCharacterMetadata{ServerVersion: version + " " + comment, Profiles: []TriggerCollationProfile{p}}))
	t.Logf("captured %d sparse BMP weight changes from %s; digest %s", len([]rune(p.From)), version, p.Digest)
}
