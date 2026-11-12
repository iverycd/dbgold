package target

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompatibilityDemoFunctionsIntegration(t *testing.T) {
	dsn := os.Getenv("DBGOLD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("DBGOLD_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := newBusinessIntegrationSchema(t, ctx, db)
	q := func(name string) string { return qualifiedFunction(schema, name) }
	w := builtinIntegrationWriter(db, schema, "postgres")
	inventory := CompatibilityRoutineNames{Functions: []string{"NEWID"}, Procedures: []string{"GetQingDanRCJGL"}}
	for _, spec := range demoCompatibilityFunctionSpecs {
		inventory.Functions = append(inventory.Functions, strings.ToUpper(spec.name))
	}
	report, err := EnsureCompatibilityFunctions(ctx, w, inventory)
	require.NoError(t, err, "installation must work before migrating business tables")
	created := make(map[string]string)
	for _, item := range report.Items {
		created[item.Name] = item.Status
	}
	for _, name := range inventory.Functions {
		require.Equal(t, "created", created[strings.ToLower(name)], name)
	}
	require.Equal(t, "created", created[demoQingDanProcedure])
	report, err = EnsureCompatibilityFunctions(ctx, w, inventory)
	require.NoError(t, err)
	require.Equal(t, report.Total, report.Reused)

	scalar := func(t *testing.T, call string, want sql.NullString) {
		t.Helper()
		var actual sql.NullString
		opening := strings.IndexByte(call, '(')
		query := "SELECT " + q(call[:opening]) + call[opening:] + "::text"
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&actual))
		require.Equal(t, want, actual, call)
	}
	value := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	t.Run("scalars_and_column_metadata", func(t *testing.T) {
		businessExec(t, ctx, db, "CREATE TABLE "+q("MyTable")+`("Second" int,"First" text)`)
		scalar(t, "fgettablecolsname('mytable')", value("Second,First"))
		scalar(t, "fgettablecolsname('missing')", sql.NullString{})
		scalar(t, "getchael(2,3)", value("0.6667"))
		scalar(t, "getchael(NULL,0)", value("0.0000"))
		scalar(t, "getchael(1,NULL)", sql.NullString{})
		scalar(t, "getwbsseg('1.2.Z',1)", value("1"))
		scalar(t, "getwbsseg('1.2.Z',2)", value("2"))
		scalar(t, "getwbsseg('1.2.Z',3)", value("90"))
		scalar(t, "getwbsseg('1.2.Z',4)", value("0"))
		scalar(t, "getwbsseg('é',1)", value("195"))
		scalar(t, "getwbsseg(NULL,1)", value("0"))
		scalar(t, "getwbsseg('1',0)", sql.NullString{})
		scalar(t, "ifkong('2026-11-12 11:22:33')", value("2026-11-12 11:22:33"))
		scalar(t, "ifkong('2026-11-12 11:22:33.4')", value("2026-11-12 11:22:33"))
		scalar(t, "ifkong('1999-12-31 23:59:59.5')", value("2000-01-01 00:00:00"))
		var same bool
		require.NoError(t, db.QueryRowContext(ctx, "SELECT "+q("ifkong")+"('')=date_trunc('second',statement_timestamp()::timestamp) AND "+q("ifkong")+"(NULL)=date_trunc('second',statement_timestamp()::timestamp)").Scan(&same))
		require.True(t, same)
		var first, second string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT "+q("newid")+"(),"+q("newid")+"()").Scan(&first, &second))
		require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, first)
		require.NotEqual(t, first, second)
	})

	t.Run("scoring", func(t *testing.T) {
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_TechLastMark")+`("BiaoDuanGuid" varchar, "DanWeiGuid" varchar,"PingFenType" varchar,"PingWeiMarkResult" numeric)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_TechLastMark")+` VALUES ('lot','bid','type',10),('lot','bid','type',10),('lot','bid','type',10),('lot','bid','type',20),('lot','bid','type',NULL),('lot','other','type',100)`)
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_Eval_LastMark")+`(biaoduanguid varchar,danweiguid varchar,techmark numeric)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_Eval_LastMark")+` VALUES ('lot','bid',32.675),('lot','other',12)`)
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_PW_Mark")+`(biaoduanguid varchar,danweiguid varchar,pfdtype varchar,pingweiguid varchar,pingweimark numeric)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_PW_Mark")+` VALUES ('lot','bid','type','a',20),('lot','bid','type','a',20),('lot','bid','type','b',15),('lot','bid','type','c',15),('lot','bid','type','d',NULL),('lot','bid','type','e',NULL),('lot','bid','other','a',200)`)
		scalar(t, "getavg_pwdf('lot','bid','type')", value("12.50"))
		scalar(t, "getavg_pwdf2('lot','bid','ignored')", value("32.68"))
		scalar(t, "getavg_pwdf2('lot','missing','type')", sql.NullString{})
		scalar(t, "getavg_pwdf3('lot','bid','type',3)", value("23.33"))
		var result string
		err := db.QueryRowContext(ctx, "SELECT "+q("getavg_pwdf3")+"('lot','bid','type',0)::text").Scan(&result)
		require.ErrorContains(t, err, "division by zero")
		scalar(t, "getavg_pwdf4('lot','bid','type','a')", value("40.00"))
		scalar(t, "getavg_pwpm('lot','bid','type')", value("1.60"))
		scalar(t, "getavg_pwpm2('lot','bid','type')", value("2.80"))
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_Eval_LastMark")+` VALUES ('lot','bid',15)`)
		err = db.QueryRowContext(ctx, "SELECT "+q("getavg_pwdf2")+"('lot','bid','type')::text").Scan(&result)
		require.ErrorContains(t, err, "more than one row")
	})

	t.Run("price_and_deviation", func(t *testing.T) {
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_Eval_DXGCFX")+`(biaoduanguid varchar,danweigcno varchar,mc varchar,je numeric)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_Eval_DXGCFX")+` VALUES ('lot','gc','item',10),('lot','gc','item',10),('lot','gc','item',20),('lot','gc','item',30),('lot','gc','only',40),('lot','other','item',15)`)
		scalar(t, "getcidxgcje('lot','gc','item')", value("20.00"))
		scalar(t, "getcidxgcje('lot','gc','only')", value("40.00"))
		scalar(t, "getcidxgcje('lot','gc','missing')", sql.NullString{})
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_PWDFSZ")+` (
 biaoduanguid varchar,
 pianli_df_y numeric,pianli_df_s_l numeric,pianli_df_x_l numeric,pianli_df_s_z numeric,pianli_df_x_z numeric,pianli_df_c numeric,
 pianli_df_df_y integer,pianli_df_df_l integer,pianli_df_df_z integer,pianli_df_df_c integer,
 pianli_pm_y numeric,pianli_pm_s_l numeric,pianli_pm_x_l numeric,pianli_pm_s_z numeric,pianli_pm_x_z numeric,pianli_pm_c numeric,
 pianli_pm_df_y integer,pianli_pm_df_l integer,pianli_pm_df_z integer,pianli_pm_df_c integer)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_PWDFSZ")+` VALUES ('lot',10,10,20,20,30,35,100,90,80,60,1,1,2,2,3,3.5,10,9,8,6)`)
		scalar(t, "getpingweidfkhjg('lot',0.0999)", value("100"))
		scalar(t, "getpingweidfkhjg('lot',0.10)", value("90"))
		scalar(t, "getpingweidfkhjieguo('lot',0.20)", value("中"))
		scalar(t, "getpingweidfkhjieguo('lot',0.30)", sql.NullString{})
		scalar(t, "getpingweipmkhjg('lot',2)", value("8"))
		scalar(t, "getpingweipmkhjieguo('lot',3.5)", value("差"))
		scalar(t, "getpingweipmkhjieguo('missing',0.1)", sql.NullString{})
	})

	t.Run("qingdan_function_and_procedure", func(t *testing.T) {
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_TB_QingDanItem")+` (
 "BiaoDuanGuid" varchar,"QingDanNo" varchar,"DanWeiGuid" varchar,"DanWeiGCNo" varchar,
 "LaborUnitPrice" numeric,"MaterialUnitPrice" numeric,"MachineUnitPrice" numeric,"OverheadUnitPrice" numeric,"Profit" numeric,
 "LaborUnitPrice_OK" numeric,"MaterialUnitPrice_OK" numeric,"MachineUnitPrice_OK" numeric,"OverheadUnitPrice_OK" numeric,"Profit_OK" numeric)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_TB_QingDanItem")+` VALUES
 ('lot','qd','bid','gc',1,2,3,4,5,10,20,30,40,50),
 ('lot','qd','bid','gc',1,2,3,4,5,10,20,30,40,50),
 ('lot','qd','peer','gc',2,4,6,8,10,16,26,36,46,56),
 ('lot','qd','excluded','gc',9,9,9,9,9,99,99,99,99,99),
 ('lot','qd','bid','other',9,9,9,9,9,99,99,99,99,99)`)
		businessExec(t, ctx, db, "CREATE TABLE "+q("PingBiao_KaiBiaoTouBiao")+`(biaoduanguid varchar,danweiguid varchar,iscalculated integer)`)
		businessExec(t, ctx, db, "INSERT INTO "+q("PingBiao_KaiBiaoTouBiao")+` VALUES ('lot','bid',1),('lot','peer',1),('lot','excluded',0)`)
		businessExec(t, ctx, db, "CREATE TABLE "+q("tmp_table")+`(marker integer)`)
		businessExec(t, ctx, db, "CREATE TABLE "+q("tmp_table2")+`(marker integer)`)
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		defer conn.Close()
		var returnCode int
		for range 2 {
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT "+q("getqingdanrcjgl")+"('lot','qd','bid','gc')").Scan(&returnCode))
			require.Zero(t, returnCode)
		}
		var count, order, unitPrice, avg int
		var name string
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_temp.tmp_table`).Scan(&count))
		require.Equal(t, 5, count)
		for _, name := range []string{"tmp_table1", "tmp_table2", "tmp_table3", "tmp_table4", "tmp_table5"} {
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_temp.`+name).Scan(&count))
			require.Equal(t, 4, count)
		}
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT unitprice::int,avg_value::int,xiangmuname,orderno FROM pg_temp.tmp_table WHERE orderno=1`).Scan(&unitPrice, &avg, &name, &order))
		require.Equal(t, 1, unitPrice)
		require.Equal(t, 12, avg)
		require.Equal(t, "人工费", name)
		require.Equal(t, 1, order)
		_, err = conn.ExecContext(ctx, "CALL "+q(demoQingDanProcedure)+"('lot','qd','peer','gc')")
		require.NoError(t, err)
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT unitprice::int,avg_value::int FROM pg_temp.tmp_table WHERE orderno=1`).Scan(&unitPrice, &avg))
		require.Equal(t, 2, unitPrice)
		require.Equal(t, 12, avg)
		_, err = conn.ExecContext(ctx, "SELECT marker FROM "+q("tmp_table"))
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "SELECT marker FROM "+q("tmp_table2"))
		require.NoError(t, err)
	})
}
