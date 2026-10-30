package source

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (r *SQLServerReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	return sqlServerPartitionMetadata(ctx, r.db, "", table)
}

func (r *SQLServerDamengReader) PartitionMetadata(ctx context.Context, table string) (*PartitionSpec, error) {
	return sqlServerPartitionMetadata(ctx, r.db, "dbo", table)
}

// A source Reader's dbName is the database, not a schema. Target inspectors
// pass the configured target schema explicitly through this helper.
func sqlServerPartitionMetadata(ctx context.Context, db *sql.DB, requestedSchema, table string) (*PartitionSpec, error) {
	schema, objectID, err := resolveSQLServerTable(ctx, db, requestedSchema, table)
	if err != nil {
		return nil, err
	}
	var function, column, typ string
	var collation sql.NullString
	var right bool
	var fanout int
	err = db.QueryRowContext(ctx, `SELECT pf.name,pf.boundary_value_on_right,pf.fanout,c.name,ty.name,c.collation_name FROM sys.indexes i JOIN sys.partition_schemes ps ON ps.data_space_id=i.data_space_id JOIN sys.partition_functions pf ON pf.function_id=ps.function_id JOIN sys.index_columns ic ON ic.object_id=i.object_id AND ic.index_id=i.index_id AND ic.partition_ordinal=1 JOIN sys.columns c ON c.object_id=ic.object_id AND c.column_id=ic.column_id JOIN sys.types ty ON ty.user_type_id=c.user_type_id WHERE i.object_id=@p1 AND i.index_id IN (0,1)`, objectID).Scan(&function, &right, &fanout, &column, &typ, &collation)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 SQL Server 原生分区函数: %w", err)
	}
	if function == "" || column == "" || fanout < 1 {
		return nil, fmt.Errorf("SQL Server 分区函数目录不完整")
	}
	spec := &PartitionSpec{Method: "RANGE", RowMovement: "enabled", Algorithm: "sqlserver-range:" + function, Keys: []PartitionKey{{Column: column, DataType: typ, Collation: collation.String}}}
	rows, err := db.QueryContext(ctx, `SELECT rv.boundary_id,CONVERT(nvarchar(128),SQL_VARIANT_PROPERTY(rv.value,'BaseType')),CONVERT(nvarchar(max),rv.value,126) FROM sys.partition_range_values rv JOIN sys.partition_functions pf ON pf.function_id=rv.function_id WHERE pf.name=@p1 ORDER BY rv.boundary_id`, function)
	if err != nil {
		return nil, err
	}
	var boundaries []PartitionValue
	for rows.Next() {
		var ordinal int
		var kind, value sql.NullString
		if err = rows.Scan(&ordinal, &kind, &value); err != nil {
			rows.Close()
			return nil, err
		}
		if ordinal != len(boundaries)+1 {
			rows.Close()
			return nil, fmt.Errorf("SQL Server 分区边界目录顺序不完整")
		}
		bound, e := catalogSQLServerBoundary(kind.String, value)
		if e != nil {
			catalogPartitionUnsupported(spec, e.Error())
		}
		boundaries = append(boundaries, bound)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(boundaries)+1 != fanout {
		return nil, fmt.Errorf("SQL Server 分区函数扇出与边界数量不一致")
	}
	for i := 0; i < fanout; i++ {
		part := Partition{Schema: schema, Name: strconv.Itoa(i + 1), Ordinal: i + 1, LowerInclusive: right, UpperInclusive: !right}
		if i == 0 {
			part.Lower = []PartitionValue{{Kind: "minvalue"}}
		} else {
			part.Lower = []PartitionValue{boundaries[i-1]}
		}
		if i == fanout-1 {
			part.Upper = []PartitionValue{{Kind: "maxvalue"}}
		} else {
			part.Upper = []PartitionValue{boundaries[i]}
		}
		spec.Partitions = append(spec.Partitions, part)
	}
	return spec, nil
}

func catalogSQLServerBoundary(kind string, value sql.NullString) (PartitionValue, error) {
	if !value.Valid {
		return PartitionValue{Kind: "null"}, nil
	}
	switch strings.ToLower(kind) {
	case "float", "real":
		// SQL Server 2008's text conversion cannot prove a lossless binary
		// floating-point boundary. Never turn rounded display text into a bound.
		return PartitionValue{}, fmt.Errorf("SQL Server 浮点分区边界缺少可证明精确的二进制读取")
	case "tinyint", "smallint", "int", "bigint", "decimal", "numeric", "money", "smallmoney", "bit":
		if !catalogPartitionNumber.MatchString(value.String) {
			return PartitionValue{}, fmt.Errorf("SQL Server 数值分区边界无法精确读取")
		}
		return PartitionValue{Kind: "number", Text: value.String}, nil
	case "char", "varchar", "nchar", "nvarchar":
		return PartitionValue{Kind: "string", Text: value.String}, nil
	case "date":
		if _, err := time.Parse("2006-01-02", value.String); err != nil {
			return PartitionValue{}, fmt.Errorf("SQL Server 日期分区边界不可识别")
		}
		return PartitionValue{Kind: "date", Text: value.String}, nil
	case "datetime", "datetime2", "smalldatetime":
		text := strings.ReplaceAll(value.String, "T", " ")
		if _, err := time.Parse("2006-01-02 15:04:05.999999999", text); err != nil {
			return PartitionValue{}, fmt.Errorf("SQL Server 时间分区边界不可识别")
		}
		return PartitionValue{Kind: "timestamp", Text: text}, nil
	default:
		return PartitionValue{}, fmt.Errorf("尚不能精确冻结的 SQL Server 分区边界类型 %s", kind)
	}
}

func (r *SQLServerReader) PartitionRoots(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *SQLServerReader) PartitionIndexes(ctx context.Context, table string) ([]IndexInfo, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT i.name,i.is_unique,i.is_primary_key,i.type_desc,ic.key_ordinal,c.name,ic.is_descending_key,CASE WHEN ip.function_id IS NULL THEN 'global' WHEN ip.function_id=bp.function_id THEN 'local' ELSE 'unaligned' END FROM sys.tables t JOIN sys.indexes i ON i.object_id=t.object_id JOIN sys.index_columns ic ON ic.object_id=i.object_id AND ic.index_id=i.index_id AND ic.key_ordinal>0 JOIN sys.columns c ON c.object_id=ic.object_id AND c.column_id=ic.column_id LEFT JOIN sys.partition_schemes ip ON ip.data_space_id=i.data_space_id LEFT JOIN sys.indexes b ON b.object_id=t.object_id AND b.index_id IN (0,1) LEFT JOIN sys.partition_schemes bp ON bp.data_space_id=b.data_space_id WHERE t.name=@p1 AND i.is_hypothetical=0 ORDER BY i.name,ic.key_ordinal`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IndexInfo
	byName := map[string]int{}
	for rows.Next() {
		var name, method, column, locality string
		var unique, primary, descending bool
		var ordinal int
		if err = rows.Scan(&name, &unique, &primary, &method, &ordinal, &column, &descending, &locality); err != nil {
			return nil, err
		}
		i, exists := byName[name]
		if !exists {
			i = len(result)
			byName[name] = i
			result = append(result, IndexInfo{TableName: table, IndexName: name, IsUnique: unique, IsPrimary: primary, Method: method, Locality: locality})
		}
		if ordinal != len(result[i].Columns)+1 || locality == "unaligned" {
			result[i].Unsupported = "分区索引的原生布局不对齐或键列顺序不完整"
		}
		result[i].Columns = append(result[i].Columns, column)
		result[i].Descending = append(result[i].Descending, descending)
	}
	return result, rows.Err()
}
