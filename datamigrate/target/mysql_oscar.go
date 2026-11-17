package target

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"slices"
	"strings"
	"time"

	"dbgold/datamigrate/dialect"
	"dbgold/datamigrate/repair"
	"dbgold/datamigrate/source"
)

// ConfigureOscarMigration inspects the effective target before running any DDL.
func (w *MySQLWriter) ConfigureOscarMigration(ctx context.Context) error {
	var version, comment, charset string
	var mode int
	var maxPacket int64
	var database sql.NullString
	query := `SELECT VERSION(),@@version_comment,@@lower_case_table_names,DATABASE(),@@character_set_connection,@@max_allowed_packet`
	if err := w.db.QueryRowContext(ctx, query).Scan(&version, &comment, &mode, &database, &charset, &maxPacket); err != nil {
		return fmt.Errorf("读取 Oscar 迁移的 MySQL 目标能力: %w", err)
	}
	if _, err := dialect.ParseMySQLVersion(version); err != nil {
		return err
	}
	product := strings.ToLower(version + " " + comment)
	for _, incompatible := range []string{"mariadb", "tidb", "oceanbase"} {
		if strings.Contains(product, incompatible) {
			return fmt.Errorf("Oscar 迁移目标要求 MySQL 5.7 或 8.0，当前为 %s (%s)", version, comment)
		}
	}
	if mode < 0 || mode > 2 {
		return fmt.Errorf("无法识别目标 MySQL lower_case_table_names=%d", mode)
	}
	if !strings.EqualFold(charset, "utf8mb4") || maxPacket <= 0 {
		return fmt.Errorf("Oscar 迁移要求目标 MySQL 连接使用 utf8mb4 和有效包大小，当前为 %s、%d 字节", charset, maxPacket)
	}
	if w.schema == "" {
		if !database.Valid || database.String == "" {
			return fmt.Errorf("Oscar 迁移必须指定目标 MySQL database")
		}
		w.schema = database.String
	}
	w.srcType, w.oscarFoldTableNames, w.oscarMySQLVersion = "oscar", mode != 0, version
	w.oscarMaxPacket = maxPacket
	return nil
}

func (w *MySQLWriter) OscarMySQLVersion() string { return w.oscarMySQLVersion }

// MySQLTableNameKey returns the name used for target table/view comparisons.
func (w *MySQLWriter) MySQLTableNameKey(name string) string {
	if w.oscarFoldTableNames {
		return strings.ToLower(name)
	}
	return name
}

func (w *MySQLWriter) validateOscarIndex(index source.IndexInfo) error {
	if w.srcType == "oscar" && strings.HasPrefix(w.oscarMySQLVersion, "5.7.") && slices.Contains(index.Descending, true) {
		return fmt.Errorf("MySQL 5.7 无法保留索引 %s 的 DESC 顺序", index.IndexName)
	}
	return nil
}

func (w *MySQLWriter) oscarSession(ctx context.Context) (*sql.Conn, func(), error) {
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	var original string
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&original); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	mode := original
	for _, required := range []string{"STRICT_ALL_TABLES", "NO_AUTO_VALUE_ON_ZERO", "NO_BACKSLASH_ESCAPES"} {
		if !strings.Contains(","+strings.ToUpper(mode)+",", ","+required+",") {
			if mode != "" {
				mode += ","
			}
			mode += required
		}
	}
	if mode == original {
		return conn, func() { _ = conn.Close() }, nil
	}
	discard := func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }) }
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode=?", mode); err != nil {
		discard()
		_ = conn.Close()
		return nil, nil, fmt.Errorf("设置 Oscar 迁移的 MySQL 严格会话: %w", err)
	}
	restore := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "SET SESSION sql_mode=?", original); err != nil {
			discard()
		}
		_ = conn.Close()
	}
	return conn, restore, nil
}

func (w *MySQLWriter) oscarAutoParameters(ctx context.Context, sequence source.SequenceInfo, targetType string) error {
	if sequence.Unsupported != "" {
		return fmt.Errorf("%s", sequence.Unsupported)
	}
	if sequence.TableName == "" || sequence.ColumnName == "" || sequence.Native != nil {
		return fmt.Errorf("仅支持已绑定到单列的 Oscar 整数自增定义")
	}
	start, err := recoveryStart(sequence)
	if err != nil || start.Sign() < 1 {
		return fmt.Errorf("Oscar 自增下一值必须是正整数")
	}
	if targetType != "" {
		bits := map[string]int{"tinyint": 7, "smallint": 15, "int": 31, "bigint": 63}[targetType]
		if bits == 0 || start.BitLen() > bits {
			return fmt.Errorf("Oscar 自增下一值 %s 超出目标 %s 的有符号整数范围", start, targetType)
		}
	}
	var increment, offset int64
	if err := w.db.QueryRowContext(ctx, `SELECT @@GLOBAL.auto_increment_increment,@@GLOBAL.auto_increment_offset`).Scan(&increment, &offset); err != nil {
		return fmt.Errorf("读取 MySQL 自增步长和偏移: %w", err)
	}
	if increment != 1 || offset != 1 {
		return fmt.Errorf("MySQL 自增步长和偏移必须为 1，当前为 %d、%d", increment, offset)
	}
	return nil
}

// ValidateOscarMySQLIdentity checks the source seed and target server settings
// before creating a table with a new AUTO_INCREMENT column.
func (w *MySQLWriter) ValidateOscarMySQLIdentity(ctx context.Context, sequence source.SequenceInfo, targetType string) error {
	if targetType == "" {
		return fmt.Errorf("Oscar 自增列缺少目标整数类型")
	}
	return w.oscarAutoParameters(ctx, sequence, targetType)
}

// ValidateOscarMySQLAutoIncrement checks an existing target before loading rows.
func (w *MySQLWriter) ValidateOscarMySQLAutoIncrement(ctx context.Context, sequence source.SequenceInfo, targetType string, load bool) error {
	if err := w.ValidateOscarMySQLIdentity(ctx, sequence, targetType); err != nil {
		return err
	}
	return w.ValidateOracleMySQLAutoIncrement(ctx, sequence, targetType, load)
}

func (w *MySQLWriter) PrepareOscarMySQLAutoIncrement(ctx context.Context, sequence source.SequenceInfo) ([]dialect.Statement, string, error) {
	if err := w.oscarAutoParameters(ctx, sequence, ""); err != nil {
		return nil, "", err
	}
	return w.PrepareOracleMySQLAutoIncrement(ctx, sequence)
}

func (w *MySQLWriter) ExecuteOscarMySQLAutoIncrement(ctx context.Context, sequence source.SequenceInfo, statements []dialect.Statement, start string) error {
	return w.ExecuteOracleMySQLAutoIncrement(ctx, sequence, statements, start)
}

// ExecuteOscarMySQLAutoRepair recalculates a saved calibration using live rows.
func (w *MySQLWriter) ExecuteOscarMySQLAutoRepair(ctx context.Context, obj repair.Object, units []repair.Unit, onStep func([]repair.Unit) error) ([]repair.Unit, error) {
	if obj.Sequence == nil {
		return units, fmt.Errorf("Oscar 自增补迁缺少冻结元数据")
	}
	if err := w.oscarAutoParameters(ctx, *obj.Sequence, ""); err != nil {
		return units, err
	}
	return w.executeOracleMySQLAutoRepair(ctx, obj, units, onStep)
}
