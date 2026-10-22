package mysqltrigger

import (
	"errors"
	"testing"

	"dbgold/datamigrate/source"
)

func TestSourceMetadataRequiredOnlyMarksMissingSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		metadata   func(map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata
		want       bool
	}{
		{"missing character metadata", "IF OLD.Note<>NEW.Note THEN SET NEW.Amount=1; END IF", nil, true},
		{"missing character column", "IF OLD.Note<>NEW.Note THEN SET NEW.Amount=1; END IF", func(tables map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata {
			m := characterFixture("SourceDB", tables)
			m.Columns = nil
			return m
		}, true},
		{"missing weight profile", "IF OLD.Note<>NEW.Note THEN SET NEW.Amount=1; END IF", func(tables map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata {
			m := characterFixture("SourceDB", tables)
			m.Profiles = nil
			return m
		}, true},
		{"missing numeric metadata", "SET NEW.Note=CONCAT(OLD.Id)", nil, true},
		{"missing numeric column", "SET NEW.Note=CONCAT(OLD.Id)", func(tables map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata {
			m := characterFixture("SourceDB", tables)
			m.NumericColumns = nil
			return m
		}, true},
		{"unsupported syntax", "CALL unsupported_routine()", nil, false},
		{"unsupported comparison", "IF OLD.Note>NEW.Note THEN SET NEW.Amount=1; END IF", nil, false},
		{"unsupported collation", "IF OLD.Note<>NEW.Note THEN SET NEW.Amount=1; END IF", func(tables map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata {
			m := characterFixture("SourceDB", tables)
			for i := range m.Columns {
				m.Columns[i].Collation = "utf8mb4_unicode_ci"
			}
			return m
		}, false},
		{"invalid weights", "IF OLD.Note<>NEW.Note THEN SET NEW.Amount=1; END IF", func(tables map[string]*source.TableDDLInfo) *source.TriggerCharacterMetadata {
			m := characterFixture("SourceDB", tables)
			m.Profiles[0].Digest = "invalid"
			return m
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables := fixture()
			var metadata *source.TriggerCharacterMetadata
			if tc.metadata != nil {
				metadata = tc.metadata(tables)
			}
			_, err := Convert(trigger(tc.body), tables, Options{Schema: "Dest", CharacterMetadata: metadata})
			if err == nil {
				t.Fatal("expected rejected conversion")
			}
			if errors.Is(err, ErrSourceMetadataRequired) != tc.want {
				t.Fatalf("unexpected metadata dependency classification: %v", err)
			}
		})
	}
}
