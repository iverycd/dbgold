package target

var demoCompatibilityProcedureSpecs = []compatibilityProcedureSpec{
	{name: "deletetoubiaoinfo", signature: "deletetoubiaoinfo(p_biaoduanguid varchar, p_danweiguid varchar)",
		inputs: []string{"character varying", "character varying"}, modes: "i,i", ddl: demoDeleteTouBiaoInfoDDL},
	{name: "deletetoubiaoinfo_sxf", signature: "deletetoubiaoinfo_sxf(p_biaoduanguid varchar, p_danweiguid varchar)",
		inputs: []string{"character varying", "character varying"}, modes: "i,i", ddl: demoDeleteTouBiaoInfoSXFDDL},
	{name: "deletezhaobiaoinfo", signature: "deletezhaobiaoinfo(p_biaoduanguid varchar, p_danweiguid varchar)",
		inputs: []string{"character varying", "character varying"}, modes: "i,i", ddl: demoDeleteZhaoBiaoInfoDDL},
	{name: "getqingdanrcjgl_proc", sourceName: "getqingdanrcjgl", signature: "getqingdanrcjgl_proc(p_biaoduanguid varchar, p_qingdanno varchar, p_danweiguid varchar, p_danweigcno varchar)",
		inputs: []string{"character varying", "character varying", "character varying", "character varying"}, modes: "i,i,i,i", ddl: demoQingDanProcedureDDL},
	{name: "proc_pb_movedata2historydb", signature: "proc_pb_movedata2historydb(p_biaoduanguid varchar)",
		inputs: []string{"character varying"}, modes: "i", ddl: demoMoveDataToHistoryDBDDL},
	{name: "proc_pingbiao_calcbjinfo", signature: "proc_pingbiao_calcbjinfo(p_bjguid varchar)",
		inputs: []string{"character varying"}, modes: "i", ddl: demoCalcBJInfoDDL},
	{name: "proc_pingbiao_deletebiaoduan", signature: "proc_pingbiao_deletebiaoduan(p_biaoduanguid varchar)",
		inputs: []string{"character varying"}, modes: "i", ddl: demoDeleteBiaoDuanDDL},
	{name: "proc_pingbiao_deletetoubiaodw", signature: "proc_pingbiao_deletetoubiaodw(p_biaoduanguid varchar, p_danweiguid varchar)",
		inputs: []string{"character varying", "character varying"}, modes: "i,i", ddl: demoDeleteTouBiaoDWDDL},
	{name: "proc_pingbiao_movealldata2historydb", signature: "proc_pingbiao_movealldata2historydb()", ddl: demoMoveAllDataToHistoryDBDDL},
	{name: "pro_getbjdetails", signature: "pro_getbjdetails(p_biaoduanguid varchar, INOUT p_result refcursor DEFAULT NULL)",
		inputs: []string{"character varying", "refcursor"}, modes: "i,b", minDefaults: 1, maxDefaults: 1, ddl: demoGetBJDetailsDDL},
	{name: "p_get_class_name", signature: "p_get_class_name(p_biaoduanguid varchar)",
		inputs: []string{"character varying"}, modes: "i", ddl: demoGetClassNameDDL},
}

const demoLotParameters = "IN p_biaoduanguid varchar"
const demoLotBidderParameters = demoLotParameters + ", IN p_danweiguid varchar"
