package target

import (
	_ "embed"
	"strings"
)

// These are the supplied definitions, not translated source routines. Keep the
// entire declaration/body suffix byte-for-byte, including AS, comments and SQL
// literals. Only the CREATE header is replaced with a qualified destination.
//
//go:embed routines/dameng_epoint_proc_alter.sql
var dmEpointProcAlterOriginal string

//go:embed routines/dameng_getcurrentnumber.sql
var dmGetCurrentNumberOriginal string

type dmBusinessRoutine struct {
	dmBusinessFunction
	kind string
}

func (r dmBusinessRoutine) label() string {
	if r.kind == "procedure" {
		return "存储过程"
	}
	return "函数"
}

func dmBusinessProcedures() []dmBusinessRoutine {
	// The first line is the supplied CREATE header; everything after it is
	// preserved. These embedded definitions are fixed application assets.
	_, alterBody, _ := strings.Cut(dmEpointProcAlterOriginal, "\n")
	_, numberBody, _ := strings.Cut(dmGetCurrentNumberOriginal, "\n")
	return []dmBusinessRoutine{
		{dmBusinessFunction: dmBusinessFunction{name: "EPOINT_PROC_ALTER", body: alterBody,
			dependencies: []dmBusinessDependency{{"API_MANAGE_CONFIG", []string{"APIGUID"}}}}, kind: "procedure"},
		{dmBusinessFunction: dmBusinessFunction{name: "GETCURRENTNUMBER",
			parameters: "NumberName_ nvarchar2,NumberFlag_ nvarchar2,FlowNumberLen number,CurrentNumber out nvarchar2", body: numberBody,
			dependencies: []dmBusinessDependency{{"CODE_MAXNUMBER", []string{"NUMBERNAME", "CURRENTVALUE", "NUMBERFLAG"}}}}, kind: "procedure"},
	}
}

func dmBusinessRoutines() []dmBusinessRoutine {
	var routines []dmBusinessRoutine
	for _, fn := range dmBusinessFunctions() {
		routines = append(routines, dmBusinessRoutine{dmBusinessFunction: fn, kind: "function"})
	}
	return append(routines, dmBusinessProcedures()...)
}

func (w *DaMengWriter) dmBusinessRoutineDDL(spec dmBusinessRoutine) string {
	if spec.kind == "function" {
		return w.dmBusinessFunctionDDL(spec.dmBusinessFunction)
	}
	return "CREATE PROCEDURE " + w.qualifiedTable(spec.name) + "(" + spec.parameters + ")\n" + spec.body
}
