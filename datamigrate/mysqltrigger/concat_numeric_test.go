package mysqltrigger

import (
	"encoding/json"
	"strings"
	"testing"

	"dbgold/datamigrate/source"
)

func numericFormatFixture(columnType string) *source.TriggerCharacterMetadata {
	return &source.TriggerCharacterMetadata{ServerVersion: "8.4.11 MySQL Community Server - GPL", NumericColumns: []source.TriggerNumericColumn{{Schema: "SourceDB", Table: "Orders", Name: "Id", ColumnType: columnType}}}
}

func TestV2ConcatNumericColumnRequiresMatchingSourceFormat(t *testing.T) {
	for _, tc := range []struct {
		dataType, columnType string
		precision, scale     int64
	}{
		{"tinyint", "tinyint(4)", 0, 0}, {"smallint", "smallint(6) unsigned", 0, 0},
		{"mediumint", "mediumint(9)", 0, 0}, {"int", "int(11) unsigned", 0, 0},
		{"integer", "int", 0, 0}, {"int", "INTEGER(11)", 0, 0}, {"bigint", "bigint unsigned", 0, 0},
		{"decimal", "decimal(8,2)", 8, 2}, {"numeric", "DECIMAL(65,30) unsigned", 65, 30},
	} {
		t.Run(tc.columnType, func(t *testing.T) {
			tables := fixture()
			col := &tables["Orders"].Columns[0]
			col.DataType, col.Precision, col.Scale = tc.dataType, tc.precision, tc.scale
			meta := numericFormatFixture(tc.columnType)
			p, err := Convert(trigger("SET NEW.Note=CONCAT('id:',OLD.Id)"), tables, Options{Schema: "Dest", CharacterMetadata: meta})
			if err != nil {
				t.Fatal(err)
			}
			if p.CharacterMetadata == nil || len(p.CharacterMetadata.NumericColumns) != 1 || len(p.CharacterMetadata.Profiles) != 0 || len(p.CharacterMetadata.Columns) != 0 {
				t.Fatalf("numeric format snapshot lost or unexpected profile retained: %+v", p.CharacterMetadata)
			}
			if err = ValidatePlan(p); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			var restored Plan
			if err = json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if err = ValidatePlan(restored); err != nil {
				t.Fatal("numeric-only snapshot did not survive save/retry", err)
			}
			meta.NumericColumns[0].ColumnType += " zerofill"
			if err = ValidatePlan(p); err != nil {
				t.Fatal("snapshot changed with caller metadata", err)
			}
		})
	}
}

func TestV2ConcatNumericColumnRejectsUnprovenFormats(t *testing.T) {
	for _, scenario := range []string{"missing metadata", "missing column", "wrong schema", "duplicate", "character conflict", "unknown version", "zerofill integer", "zerofill decimal", "wrong base", "wrong precision", "wrong scale", "decimal unspecified", "decimal omitted scale", "integer scale", "float", "extra modifier"} {
		t.Run(scenario, func(t *testing.T) {
			tables := fixture()
			meta := numericFormatFixture("int(11)")
			if strings.Contains(scenario, "decimal") || scenario == "wrong precision" || scenario == "wrong scale" {
				tables["Orders"].Columns[0].DataType = "decimal"
				tables["Orders"].Columns[0].Precision, tables["Orders"].Columns[0].Scale = 8, 2
				meta.NumericColumns[0].ColumnType = "decimal(8,2)"
			}
			switch scenario {
			case "missing metadata":
				meta = nil
			case "missing column":
				meta.NumericColumns = nil
			case "wrong schema":
				meta.NumericColumns[0].Schema = "OtherDB"
			case "duplicate":
				meta.NumericColumns = append(meta.NumericColumns, meta.NumericColumns[0])
			case "character conflict":
				meta.Columns = []source.TriggerColumnCharacter{{Schema: "SourceDB", Table: "Orders", Name: "Id", Charset: "utf8mb4", Collation: "utf8mb4_general_ci"}}
			case "unknown version":
				meta.ServerVersion = "10.11.0 MariaDB"
			case "zerofill integer":
				meta.NumericColumns[0].ColumnType = "int(6) unsigned zerofill"
			case "zerofill decimal":
				meta.NumericColumns[0].ColumnType = "decimal(8,2) unsigned zerofill"
			case "wrong base":
				meta.NumericColumns[0].ColumnType = "bigint"
			case "wrong precision":
				meta.NumericColumns[0].ColumnType = "decimal(10,2)"
			case "wrong scale":
				meta.NumericColumns[0].ColumnType = "decimal(8,3)"
			case "decimal unspecified":
				meta.NumericColumns[0].ColumnType = "decimal"
			case "decimal omitted scale":
				meta.NumericColumns[0].ColumnType = "decimal(8)"
			case "integer scale":
				meta.NumericColumns[0].ColumnType = "int(11,2)"
			case "float":
				meta.NumericColumns[0].ColumnType = "float"
			case "extra modifier":
				meta.NumericColumns[0].ColumnType = "int signed"
			}
			if _, err := Convert(trigger("SET NEW.Note=CONCAT(OLD.Id)"), tables, Options{Schema: "Dest", CharacterMetadata: meta}); err == nil {
				t.Fatal("accepted an unproven numeric string format")
			}
		})
	}
}

func TestV2ConcatNumericMetadataFreezesOnlyUsedColumns(t *testing.T) {
	tables := fixture()
	meta := characterFixture("SourceDB", tables)
	// Unrelated profile failures do not matter to numeric output formatting.
	meta.Profiles[0].Digest = "broken"
	p, err := Convert(trigger("SET NEW.Note=CONCAT(OLD.Id,NEW.Id)"), tables, Options{Schema: "Dest", CharacterMetadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	if p.CharacterMetadata == nil || len(p.CharacterMetadata.NumericColumns) != 1 || p.CharacterMetadata.NumericColumns[0].Name != "Id" || len(p.CharacterMetadata.Profiles) != 0 || len(p.CharacterMetadata.Columns) != 0 {
		t.Fatalf("unrelated metadata retained: %+v", p.CharacterMetadata)
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	p.CharacterMetadata.NumericColumns[0].ColumnType = "int(6) unsigned zerofill"
	if err = ValidatePlan(p); err == nil {
		t.Fatal("saved SQL validated with a changed ZEROFILL format")
	}
	for _, body := range []string{"SET NEW.Amount=OLD.Id", "IF OLD.Id!=NEW.Id THEN SET NEW.Amount=1; END IF", "SET NEW.Note=CONCAT(1,1.20,NULL)"} {
		p, err = Convert(trigger(body), tables, Options{Schema: "Dest"})
		if err != nil {
			t.Fatal("ordinary numeric expressions or numeric constants acquired a format dependency", err)
		}
		if p.CharacterMetadata != nil {
			t.Fatal("unused numeric metadata was frozen")
		}
	}
}

func TestV2ConcatNumericSelectColumnsFreezeReadTableFormats(t *testing.T) {
	tables := fixture()
	tables["Audit"].Columns[1].Precision, tables["Audit"].Columns[1].Scale = 8, 2
	meta := characterFixture("SourceDB", tables)
	i := trigger("INSERT INTO Audit(Note) SELECT CONCAT(Id, ':', Amount, ':', NEW.Id) FROM Audit WHERE Id=NEW.Id")
	p, err := Convert(i, tables, Options{Schema: "Dest", CharacterMetadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	if p.CharacterMetadata == nil || len(p.CharacterMetadata.NumericColumns) != 3 || len(p.CharacterMetadata.Profiles) != 0 {
		t.Fatalf("wrong read-table numeric dependencies: %+v", p.CharacterMetadata)
	}
	if !strings.Contains(p.FunctionBody, `CAST(CAST("Audit"."Amount" AS numeric(8,2)) AS text)`) {
		t.Fatal("read-table decimal scale was not preserved", p.FunctionBody)
	}
	if err = ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
}
