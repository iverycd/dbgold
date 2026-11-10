package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

// MySQLSpatialSubtype identifies MySQL spatial declarations and protocol types.
// Object names are never normalized here; only the data type is classified.
func MySQLSpatialSubtype(dataType string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(dataType)) {
	case "GEOMETRY":
		return "Geometry", true
	case "POINT":
		return "Point", true
	case "LINESTRING":
		return "LineString", true
	case "POLYGON":
		return "Polygon", true
	case "MULTIPOINT":
		return "MultiPoint", true
	case "MULTILINESTRING":
		return "MultiLineString", true
	case "MULTIPOLYGON":
		return "MultiPolygon", true
	case "GEOMETRYCOLLECTION", "GEOMCOLLECTION":
		return "GeometryCollection", true
	default:
		return "", false
	}
}

// MySQLSpatialIndexUnsupported is retained by the Reader until a target-specific
// planner verifies that the complete spatial definition can be represented.
const MySQLSpatialIndexUnsupported = "暂不支持 MySQL 空间索引的等价转换，请根据源端定义手工适配"

// IsMySQLSpatialIndex accepts both the DDL name and the actual R-tree method
// reported by INFORMATION_SCHEMA.STATISTICS on MySQL server versions.
func IsMySQLSpatialIndex(method string) bool {
	return strings.EqualFold(strings.TrimSpace(method), "SPATIAL") || strings.EqualFold(strings.TrimSpace(method), "RTREE")
}

type SpatialMetadataQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (r *MySQLReader) loadMySQLSpatialSRIDs(ctx context.Context, info *TableDDLInfo) error {
	return LoadMySQLSpatialSRIDs(ctx, r, r.dbName, info)
}

// LoadMySQLSpatialSRIDs reads column-level restrictions separately so old MySQL releases and ordinary
// tables retain their existing metadata query. Only an absent SRS_ID column is
// a compatibility fallback; permissions and connection errors must propagate.
// CDC uses the same lookup, ensuring startup and full-copy compatibility checks
// distinguish unrestricted columns from an explicit zero restriction.
func LoadMySQLSpatialSRIDs(ctx context.Context, db SpatialMetadataQueryer, schema string, info *TableDDLInfo) error {
	spatial := make(map[string]int)
	for i, col := range info.Columns {
		if _, ok := MySQLSpatialSubtype(col.DataType); ok {
			spatial[col.Name] = i
		}
	}
	if len(spatial) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, SRS_ID FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, schema, info.TableName)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1054 && strings.Contains(strings.ToUpper(mysqlErr.Message), "SRS_ID") {
			return nil
		}
		return fmt.Errorf("读取空间列 SRID [%s]: %w", info.TableName, err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var name string
		var srid sql.NullInt64
		if err := rows.Scan(&name, &srid); err != nil {
			return err
		}
		i, ok := spatial[name]
		if !ok {
			continue
		}
		seen[name] = true
		if srid.Valid {
			if srid.Int64 < 0 || srid.Int64 > math.MaxUint32 {
				return fmt.Errorf("空间列 %s.%s 的 SRID %d 超出 MySQL 范围", info.TableName, name, srid.Int64)
			}
			value := uint32(srid.Int64)
			info.Columns[i].SpatialSRID = &value
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name := range spatial {
		if !seen[name] {
			return fmt.Errorf("空间列 %s.%s 的 SRID 元数据缺失", info.TableName, name)
		}
	}
	return nil
}
