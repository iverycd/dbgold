package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "gitee.com/opengauss/openGauss-connector-go-pq"
	gomysql "github.com/go-sql-driver/mysql"
)

type databases struct {
	mysql *sql.DB
	gauss *sql.DB
}

func openDatabases(ctx context.Context, cfg Config, mysqlDatabase bool) (*databases, error) {
	mysqlPassword, err := envRequired("CDCSTRESS_MYSQL_PASSWORD")
	if err != nil {
		return nil, err
	}
	gaussPassword, err := envRequired("CDCSTRESS_GAUSSDB_PASSWORD")
	if err != nil {
		return nil, err
	}
	dbName := ""
	if mysqlDatabase {
		dbName = cfg.MySQL.Database
	}
	mysqlDSN := mysqlDSN(cfg, mysqlPassword, dbName)
	gaussURL := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(cfg.GaussDB.Host, strconv.Itoa(cfg.GaussDB.Port)), Path: cfg.GaussDB.Database,
		User: url.UserPassword(cfg.GaussDB.User, gaussPassword), RawQuery: url.Values{"sslmode": []string{cfg.GaussDB.SSLMode}}.Encode()}
	gaussDSN := gaussURL.String()
	mysqlDB, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		return nil, err
	}
	gaussDB, err := sql.Open("opengauss", gaussDSN)
	if err != nil {
		mysqlDB.Close()
		return nil, err
	}
	pool := cfg.resolvedPool()
	for _, db := range []*sql.DB{mysqlDB, gaussDB} {
		configureDBPool(db, pool)
	}
	dbs := &databases{mysql: mysqlDB, gauss: gaussDB}
	for name, db := range map[string]*sql.DB{"mysql": mysqlDB, "gaussdb": gaussDB} {
		pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err != nil {
			dbs.close()
			return nil, fmt.Errorf("connect %s: %w", name, err)
		}
	}
	return dbs, nil
}

func configureDBPool(db *sql.DB, pool resolvedPoolConfig) {
	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)
	db.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
}

func (d *databases) close() {
	if d == nil {
		return
	}
	if d.mysql != nil {
		_ = d.mysql.Close()
	}
	if d.gauss != nil {
		_ = d.gauss.Close()
	}
}

func mysqlIdent(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
func pgIdent(name string) string    { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func mysqlAdminDSN(cfg Config, password string) string {
	return mysqlDSN(cfg, password, "")
}

func mysqlDSN(cfg Config, password, database string) string {
	dsn := gomysql.NewConfig()
	dsn.User, dsn.Passwd = cfg.MySQL.User, password
	dsn.Net, dsn.Addr, dsn.DBName = "tcp", net.JoinHostPort(cfg.MySQL.Host, strconv.Itoa(cfg.MySQL.Port)), database
	dsn.ParseTime = true
	dsn.Params = map[string]string{"charset": "utf8mb4"}
	return dsn.FormatDSN()
}

func prepare(ctx context.Context, cfg Config, state *RunState, target bool) error {
	if state.ActiveJobID != "" {
		return fmt.Errorf("cannot prepare while job %s is active", state.ActiveJobID)
	}
	if state.Prepared {
		log.Printf("run %s is already prepared; leaving database contents unchanged", state.RunID)
		return nil
	}
	password, err := envRequired("CDCSTRESS_MYSQL_PASSWORD")
	if err != nil {
		return err
	}
	admin, err := sql.Open("mysql", mysqlAdminDSN(cfg, password))
	if err != nil {
		return err
	}
	if err = admin.PingContext(ctx); err != nil {
		admin.Close()
		return fmt.Errorf("connect mysql: %w", err)
	}
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+mysqlIdent(cfg.MySQL.Database)+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		admin.Close()
		return fmt.Errorf("create source database: %w", err)
	}
	noiseDatabase := cfg.MySQL.Database + "_noise"
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+mysqlIdent(noiseDatabase)+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		admin.Close()
		return fmt.Errorf("create noise database: %w", err)
	}
	if _, err = admin.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+mysqlIdent(noiseDatabase)+"."+mysqlIdent("cdcstress_cross_database_noise")+" (id BIGINT PRIMARY KEY, payload VARCHAR(64) NOT NULL) ENGINE=InnoDB"); err != nil {
		admin.Close()
		return fmt.Errorf("create cross-database noise table: %w", err)
	}
	admin.Close()
	dbs, err := openDatabases(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer dbs.close()
	if err = ensureGaussSchema(ctx, dbs.gauss, cfg.GaussDB.Schema); err != nil {
		return fmt.Errorf("create target schema: %w", err)
	}
	if _, err = dbs.mysql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cdcstress_noise_outside_scope (
		id BIGINT PRIMARY KEY, payload VARCHAR(64) NOT NULL) ENGINE=InnoDB`); err != nil {
		return fmt.Errorf("create scope-isolation table: %w", err)
	}
	if _, err = dbs.mysql.ExecContext(ctx, `INSERT INTO cdcstress_noise_outside_scope(id,payload) VALUES (1,'must-not-migrate')
		ON DUPLICATE KEY UPDATE payload=VALUES(payload)`); err != nil {
		return fmt.Errorf("seed scope-isolation table: %w", err)
	}
	if len(state.Tables) == 0 {
		state.Tables, err = buildProfile(cfg.Profile, state.RunID)
		if err != nil {
			return err
		}
		if err = saveState(cfg, state); err != nil {
			return err
		}
	}
	log.Printf("preparing %d tables and %d rows", len(state.Tables), cfg.Profile.TotalRows)
	jobs := make(chan TableSpec)
	errCh := make(chan error, cfg.Profile.Workers)
	var wg sync.WaitGroup
	for worker := 0; worker < cfg.Profile.Workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for table := range jobs {
				if e := prepareSourceTable(ctx, dbs.mysql, cfg, state.RunID, table); e != nil {
					select {
					case errCh <- e:
					default:
					}
					return
				}
				if target {
					if e := prepareTargetTable(ctx, dbs.gauss, cfg, state.RunID, table); e != nil {
						select {
						case errCh <- e:
						default:
						}
						return
					}
				}
			}
		}()
	}
	for _, table := range state.Tables {
		select {
		case jobs <- table:
		case err = <-errCh:
			close(jobs)
			wg.Wait()
			return err
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err = <-errCh:
		return err
	default:
	}
	state.Prepared = true
	return saveState(cfg, state)
}

// Older GaussDB/openGauss versions do not accept CREATE SCHEMA IF NOT EXISTS.
// Querying the catalog first keeps preparation repeatable across versions.
func ensureGaussSchema(ctx context.Context, db *sql.DB, schema string) error {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := db.ExecContext(ctx, "CREATE SCHEMA "+pgIdent(schema))
	return err
}

func prepareSourceTable(ctx context.Context, db *sql.DB, cfg Config, runID string, table TableSpec) error {
	var count int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mysqlIdent(table.Name)).Scan(&count)
	if err == nil && count == table.Rows {
		return nil
	}
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "doesn't exist") && !strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		return fmt.Errorf("inspect %s: %w", table.Name, err)
	}
	if _, err = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+mysqlIdent(table.Name)); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, mysqlCreateTable(table)); err != nil {
		return fmt.Errorf("create %s: %w", table.Name, err)
	}
	for start := int64(1); start <= table.Rows; start += int64(cfg.Profile.BatchSize) {
		end := min(start+int64(cfg.Profile.BatchSize)-1, table.Rows)
		query, args := mysqlInsertBatch(table, runID, start, end)
		if _, err = db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("seed %s rows %d-%d: %w", table.Name, start, end, err)
		}
	}
	return nil
}

func commonMySQLColumns() string {
	return "id BIGINT NOT NULL, tenant_id INT NOT NULL, code VARCHAR(64) NOT NULL, event_id BIGINT NOT NULL, payload VARCHAR(255) NOT NULL, amount DECIMAL(18,4) NOT NULL, score DOUBLE NOT NULL, active TINYINT(1) NOT NULL, created_at DATETIME(6) NOT NULL, note TEXT NULL, blob_data VARBINARY(64) NULL"
}

func mysqlCreateTable(table TableSpec) string {
	columns := commonMySQLColumns()
	constraint := ""
	switch table.Kind {
	case kindPrimary:
		columns = strings.Replace(columns, "id BIGINT NOT NULL", "id BIGINT NOT NULL AUTO_INCREMENT", 1)
		constraint = ", PRIMARY KEY(id)"
	case kindComposite:
		constraint = ", PRIMARY KEY(tenant_id,id)"
	case kindUnique:
		constraint = ", UNIQUE KEY uq_code(code)"
	case kindWide:
		columns += ", extra_int BIGINT NULL, extra_text VARCHAR(512) NULL"
		constraint = ", PRIMARY KEY(id)"
	case kindMixedCase:
		columns += ", `order` VARCHAR(32) NULL"
		constraint = ", PRIMARY KEY(id)"
	}
	return "CREATE TABLE " + mysqlIdent(table.Name) + " (" + columns + constraint + ") ENGINE=InnoDB"
}

func mysqlInsertBatch(table TableSpec, runID string, start, end int64) (string, []any) {
	columns := []string{"id", "tenant_id", "code", "event_id", "payload", "amount", "score", "active", "created_at", "note", "blob_data"}
	if table.Kind == kindWide {
		columns = append(columns, "extra_int", "extra_text")
	}
	if table.Kind == kindMixedCase {
		columns = append(columns, "order")
	}
	marks := make([]string, 0, end-start+1)
	args := make([]any, 0, int(end-start+1)*len(columns))
	for row := start; row <= end; row++ {
		values := rowValues(table, runID, row)
		marks = append(marks, "("+strings.TrimRight(strings.Repeat("?,", len(values)), ",")+")")
		args = append(args, values...)
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = mysqlIdent(column)
	}
	return "INSERT INTO " + mysqlIdent(table.Name) + " (" + strings.Join(quoted, ",") + ") VALUES " + strings.Join(marks, ","), args
}

func rowValues(table TableSpec, runID string, row int64) []any {
	tenant := int(row%31 + 1)
	code := fmt.Sprintf("t%06d-r%012d", table.Index, row)
	payload := fmt.Sprintf("%s/%s/%d/中文/emoji-🚀", runID, table.Name, row)
	note := any(nil)
	if row%7 != 0 {
		note = fmt.Sprintf("note-%d", row%1000)
	}
	if row%1000 == 0 {
		note = strings.Repeat("长文本-CDC-", 256)
	}
	blob := []byte(fmt.Sprintf("%s:%d", table.Name, row))
	values := []any{row, tenant, code, row, payload, fmt.Sprintf("%d.%04d", row%1_000_000, row%10_000), float64(row % 1000), row % 2, time.Unix(1_700_000_000+row%10_000_000, int64(row%1_000_000)*1000).UTC(), note, blob}
	if table.Kind == kindWide {
		values = append(values, row*3, strings.Repeat("w", int(row%32)+1))
	}
	if table.Kind == kindMixedCase {
		values = append(values, fmt.Sprintf("order-%d", row%100))
	}
	return values
}

func prepareTargetTable(ctx context.Context, db *sql.DB, cfg Config, runID string, table TableSpec) error {
	name := table.Name
	if cfg.Profile.LowerCaseNames {
		name = strings.ToLower(name)
	}
	qualified := pgIdent(cfg.GaussDB.Schema) + "." + pgIdent(name)
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+qualified+" CASCADE"); err != nil {
		return err
	}
	columns := "id BIGINT NOT NULL, tenant_id INTEGER NOT NULL, code VARCHAR(64) NOT NULL, event_id BIGINT NOT NULL, payload VARCHAR(255) NOT NULL, amount NUMERIC(18,4) NOT NULL, score NUMERIC(22,0) NOT NULL, active INTEGER NOT NULL, created_at TIMESTAMP NOT NULL, note TEXT NULL, blob_data BYTEA NULL"
	constraint := ""
	switch table.Kind {
	case kindPrimary:
		constraint = ", PRIMARY KEY(id)"
	case kindComposite:
		constraint = ", PRIMARY KEY(tenant_id,id)"
	case kindUnique:
		constraint = ", UNIQUE(code)"
	case kindWide:
		columns += ", extra_int BIGINT NULL, extra_text VARCHAR(512) NULL"
		constraint = ", PRIMARY KEY(id)"
	case kindMixedCase:
		columns += ", " + pgIdent("order") + " VARCHAR(32) NULL"
		constraint = ", PRIMARY KEY(id)"
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+qualified+" ("+columns+constraint+")"); err != nil {
		return err
	}
	if table.Kind == kindPrimary {
		sequence := "seq_" + name + "_id"
		qualifiedSequence := pgIdent(cfg.GaussDB.Schema) + "." + pgIdent(sequence)
		for _, statement := range []string{
			"DROP SEQUENCE IF EXISTS " + qualifiedSequence + " CASCADE",
			"CREATE SEQUENCE " + qualifiedSequence + " INCREMENT BY 1 START 1",
			"ALTER TABLE " + qualified + " ALTER COLUMN " + pgIdent("id") + " SET DEFAULT nextval('" + qualifiedSequence + "')",
			"ALTER SEQUENCE " + qualifiedSequence + " OWNED BY " + qualified + "." + pgIdent("id"),
		} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("prepare target sequence for %s: %w", table.Name, err)
			}
		}
	}
	for start := int64(1); start <= table.Rows; start += int64(cfg.Profile.BatchSize) {
		end := min(start+int64(cfg.Profile.BatchSize)-1, table.Rows)
		query, args := pgInsertBatch(cfg, table, runID, start, end)
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("seed target %s rows %d-%d: %w", table.Name, start, end, err)
		}
	}
	return nil
}

func pgInsertBatch(cfg Config, table TableSpec, runID string, start, end int64) (string, []any) {
	columns := commonColumnNames(table)
	marks := make([]string, 0, end-start+1)
	args := make([]any, 0, int(end-start+1)*len(columns))
	parameter := 1
	for row := start; row <= end; row++ {
		values := rowValues(table, runID, row)
		rowMarks := make([]string, len(values))
		for i := range values {
			rowMarks[i] = fmt.Sprintf("$%d", parameter)
			parameter++
		}
		marks = append(marks, "("+strings.Join(rowMarks, ",")+")")
		args = append(args, values...)
	}
	name := table.Name
	if cfg.Profile.LowerCaseNames {
		name = strings.ToLower(name)
	}
	return "INSERT INTO " + pgIdent(cfg.GaussDB.Schema) + "." + pgIdent(name) + " (" + strings.Join(quoteColumns(columns, pgIdent), ",") + ") VALUES " + strings.Join(marks, ","), args
}

func cleanup(ctx context.Context, cfg Config, state RunState, confirmation string) error {
	if confirmation != state.RunID || !safeNamespace(cfg.MySQL.Database) || !safeNamespace(cfg.GaussDB.Schema) {
		return fmt.Errorf("cleanup safety check failed")
	}
	if state.ActiveJobID != "" {
		return fmt.Errorf("run still records active job %s; stop or abort it before cleanup", state.ActiveJobID)
	}
	for _, table := range state.Tables {
		if !strings.HasPrefix(strings.ToLower(table.Name), objectPrefix) {
			return fmt.Errorf("manifest contains unsafe table name %q", table.Name)
		}
	}
	dbs, err := openDatabases(ctx, cfg, true)
	if err != nil {
		return err
	}
	for _, table := range state.Tables {
		if _, err = dbs.mysql.ExecContext(ctx, "DROP TABLE IF EXISTS "+mysqlIdent(table.Name)); err != nil {
			dbs.close()
			return err
		}
	}
	if _, err = dbs.gauss.ExecContext(ctx, "DROP SCHEMA "+pgIdent(cfg.GaussDB.Schema)+" CASCADE"); err != nil {
		dbs.close()
		return err
	}
	dbs.close()
	password, err := envRequired("CDCSTRESS_MYSQL_PASSWORD")
	if err != nil {
		return err
	}
	admin, err := sql.Open("mysql", mysqlAdminDSN(cfg, password))
	if err != nil {
		return err
	}
	defer admin.Close()
	if _, err = admin.ExecContext(ctx, "DROP DATABASE "+mysqlIdent(cfg.MySQL.Database)); err != nil {
		return fmt.Errorf("drop source database: %w", err)
	}
	if _, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+mysqlIdent(cfg.MySQL.Database+"_noise")); err != nil {
		return fmt.Errorf("drop source noise database: %w", err)
	}
	log.Printf("cleaned %d source tables and target schema %s for run %s", len(state.Tables), cfg.GaussDB.Schema, state.RunID)
	return nil
}

func redactedDSN(cfg DatabaseConfig) string {
	u := &url.URL{Scheme: "db", Host: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), Path: cfg.Database}
	u.User = url.User(cfg.User)
	return u.Redacted()
}

func bytesHex(value []byte) string { return hex.EncodeToString(value) }
