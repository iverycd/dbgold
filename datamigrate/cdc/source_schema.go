package cdc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const SourceSchemaVersion = 1
const LegacyDamengSchemaMessage = "达梦 CDC 源结构快照已升级，旧任务不能恢复，请重新建立全量或增量起点"

// SourceSchemaTable explicitly persists fields intentionally omitted from the
// public TableInfo response. Column order is part of the binlog wire format.
type SourceSchemaTable struct {
	TableInfo
	Definitions  []string          `json:"definitions"`
	Charsets     []string          `json:"charsets"`
	SpatialSRIDs []*uint32         `json:"spatial_srids"`
	Indexes      []UniqueIndexInfo `json:"unique_indexes"`
}
type SourceSchemaSnapshot struct {
	Version  int                 `json:"version"`
	Database string              `json:"database"`
	Tables   []SourceSchemaTable `json:"tables"`
	Hash     string              `json:"hash"`
}

func NewSourceSchema(database string, tables []TableInfo) (SourceSchemaSnapshot, error) {
	s := SourceSchemaSnapshot{Version: SourceSchemaVersion, Database: database}
	for _, t := range tables {
		// Row locators have their own frozen manifest. They are not source DDL.
		t.LocatorStrategy, t.LocatorIndex, t.LocatorWarning, t.LocatorColumns = "", "", "", nil
		s.Tables = append(s.Tables, SourceSchemaTable{t, t.ColumnDefinitions, t.ColumnCharsets, t.ColumnSpatialSRIDs, t.UniqueIndexes})
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	raw, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	// Own all slices: subsequent locator resolution must not mutate the snapshot.
	var owned SourceSchemaSnapshot
	if err = json.Unmarshal(raw, &owned); err != nil {
		return s, err
	}
	s = owned
	s.Hash = s.digest()
	return s, s.Validate(database)
}
func (s SourceSchemaSnapshot) digest() string {
	s.Hash = ""
	raw, _ := json.Marshal(s)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s SourceSchemaSnapshot) Validate(database string) error {
	if s.Version != SourceSchemaVersion {
		return fmt.Errorf("%s", LegacyDamengSchemaMessage)
	}
	if s.Database != database || s.Hash == "" || s.Hash != s.digest() || len(s.Tables) == 0 {
		return fmt.Errorf("CDC 源结构快照缺失、数据库不匹配或摘要校验失败")
	}
	seen := map[string]bool{}
	for _, t := range s.Tables {
		if t.Name == "" || seen[t.Name] || len(t.Columns) == 0 || len(t.Columns) != len(t.ColumnTypes) || len(t.Definitions) != len(t.Columns) || len(t.Charsets) != len(t.Columns) {
			return fmt.Errorf("表 %s 的 CDC 源结构快照不完整", t.Name)
		}
		seen[t.Name] = true
		cols := map[string]bool{}
		for _, col := range t.Columns {
			if col == "" || cols[col] {
				return fmt.Errorf("表 %s 的快照列无效", t.Name)
			}
			cols[col] = true
		}
		for _, indexes := range [][]int{t.PrimaryKey, t.AutoIncrement} {
			for _, i := range indexes {
				if i < 0 || i >= len(t.Columns) {
					return fmt.Errorf("表 %s 的快照列序号无效", t.Name)
				}
			}
		}
	}
	return nil
}
func (s SourceSchemaSnapshot) TableInfos(database string, names []string) ([]TableInfo, error) {
	if err := s.Validate(database); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var copy SourceSchemaSnapshot
	if err = json.Unmarshal(raw, &copy); err != nil {
		return nil, err
	}
	byName := map[string]TableInfo{}
	for _, saved := range copy.Tables {
		t := saved.TableInfo
		t.ColumnDefinitions, t.ColumnCharsets, t.ColumnSpatialSRIDs, t.UniqueIndexes = saved.Definitions, saved.Charsets, saved.SpatialSRIDs, saved.Indexes
		byName[t.Name] = t
	}
	if names == nil {
		for _, t := range copy.Tables {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("有效 CDC 表清单为空")
	}
	result := make([]TableInfo, 0, len(names))
	for _, name := range names {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("源结构快照缺少表 %s", name)
		}
		result = append(result, t)
	}
	return result, nil
}
func (s SourceSchemaSnapshot) Select(database string, names []string) (SourceSchemaSnapshot, error) {
	tables, err := s.TableInfos(database, names)
	if err != nil {
		return SourceSchemaSnapshot{}, err
	}
	return NewSourceSchema(database, tables)
}

// CaptureSourceSchema proves that the current metadata also describes start.
// Head is captured AFTER metadata; any intervening DDL makes the proof fail.
func CaptureSourceSchema(ctx context.Context, cfg Config, start Position) (SourceSchemaSnapshot, error) {
	db, err := OpenSource(cfg.SourceDSN)
	if err != nil {
		return SourceSchemaSnapshot{}, err
	}
	defer db.Close()
	if err = ValidatePositionAvailable(ctx, cfg, start); err != nil {
		return SourceSchemaSnapshot{}, err
	}
	tables, err := LoadConfiguredTables(ctx, db, cfg)
	if err != nil {
		return SourceSchemaSnapshot{}, err
	}
	head, err := CurrentPosition(ctx, db)
	if err != nil {
		return SourceSchemaSnapshot{}, err
	}
	if err = validateSchemaInterval(ctx, cfg, start, head); err != nil {
		return SourceSchemaSnapshot{}, err
	}
	return NewSourceSchema(cfg.SourceDatabase, tables)
}
