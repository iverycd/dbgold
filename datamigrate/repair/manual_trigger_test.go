package repair

import (
	"testing"

	"dbgold/datamigrate/damengnative"
	"github.com/stretchr/testify/require"
)

func TestManualTriggerExecutionRequiresStoredIdentityAndUnits(t *testing.T) {
	query := `CREATE TRIGGER APP.A AFTER INSERT ON APP.T BEGIN INSERT INTO APP.LOG VALUES(1); END;`
	original := Object{Kind: "trigger", Schema: "APP", TargetName: "A", Table: "T", Status: "failed", DiagnosticOnly: true}
	o := original
	o.DiagnosticOnly = false
	o.ManualTrigger = &ManualTriggerPlan{SQL: query, Revision: 1, Original: &original}
	o.Units = []Unit{{SQL: query, OriginalSQL: query}}
	require.True(t, o.TrustedManualTrigger())
	require.True(t, o.CanEditDDL())
	require.True(t, o.CanExecuteDDL())
	o.ManualTriggerEditable = false
	require.True(t, o.CanExecuteDDL())
	o.Table = "OTHER"
	require.False(t, o.CanExecuteDDL())
	o.Table = "T"
	o.Units[0].SQL += " "
	require.False(t, o.CanExecuteDDL())
	o.Units[0].SQL = query
	native := &damengnative.Object{Kind: "trigger", Name: "A", Schema: "APP", Table: "T", Units: []damengnative.Unit{{SQL: query, Phase: "after_data"}}}
	o.Native = native
	o.NativeFingerprint = damengnative.Fingerprint(native)
	require.True(t, o.CanExecuteDDL())
	o.NativeFingerprint = "changed"
	require.False(t, o.CanExecuteDDL())
}

func TestResponseCapabilityCannotAuthorizeManualSQL(t *testing.T) {
	o := Object{Kind: "trigger", Schema: "APP", TargetName: "A", Table: "T", ManualTriggerEditable: true}
	require.False(t, o.CanEditDDL())
	require.False(t, o.CanExecuteDDL())
}
