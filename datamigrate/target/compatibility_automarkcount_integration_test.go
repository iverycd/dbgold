package target

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutoMarkCountSQLIntegration(t *testing.T) {
	testPGCompatibilityProducts(t, func(t *testing.T, ctx context.Context, db *sql.DB, product string) {
		schema := newBusinessIntegrationSchema(t, ctx, db)
		report, err := EnsureCompatibilityFunctions(ctx, builtinIntegrationWriter(db, schema, product), CompatibilityRoutineNames{Procedures: []string{"AutoMarkCount"}})
		require.NoError(t, err, "initialization must succeed before the exam tables exist")
		require.Equal(t, "automarkcount", report.Items[len(report.Items)-1].Name)
		require.Equal(t, "created", report.Items[len(report.Items)-1].Status)

		examUser := qualifiedFunction(schema, "ExAmUsErLiSt")
		exam := qualifiedFunction(schema, "Examlist")
		question := qualifiedFunction(schema, "examquestionlist")
		result := qualifiedFunction(schema, "ExamResultList")
		answer := qualifiedFunction(schema, "examanswer")
		businessExec(t, ctx, db, "CREATE TABLE "+examUser+` (
			"ExamGuid" varchar, "UserGuid" varchar, "CoDe" integer CHECK ("CoDe" <= 100),
			"EndDate" timestamp, "ExamIsPass" integer, PRIMARY KEY ("ExamGuid","UserGuid"))`)
		businessExec(t, ctx, db, "CREATE TABLE "+exam+` (
			"EXAMGUID" varchar PRIMARY KEY, singlescore integer, "MultiScore" integer, judgescore integer)`)
		businessExec(t, ctx, db, "CREATE TABLE "+question+` (
			examguid varchar, "SubjectGuid" varchar, "IsCheck" varchar)`)
		businessExec(t, ctx, db, "CREATE TABLE "+result+` (
			"ExamGUID" varchar, userguid varchar, "SUBJECTGUID" varchar, "AnswerID" varchar)`)
		businessExec(t, ctx, db, "CREATE TABLE "+answer+` (
			subjectguid varchar, "ISRIGHT" integer, "AnswerGuid" varchar)`)
		businessExec(t, ctx, db, "INSERT INTO "+exam+` VALUES ('exam',2,3,5)`)
		businessExec(t, ctx, db, "INSERT INTO "+question+` VALUES
			('exam','single','0'), ('exam','multi','1'), ('exam','judge','2'),
			('exam','partial','1'), ('exam','subjective','3'), ('no-config','single','0')`)
		businessExec(t, ctx, db, "INSERT INTO "+answer+` VALUES
			('single',1,'single-guid'), ('multi',1,'multi-1'), ('multi',1,'multi-2'),
			('multi',0,'wrong'), ('judge',1,'yes'), ('partial',1,'one'), ('partial',1,'two'),
			('subjective',1,'subjective-guid')`)
		businessExec(t, ctx, db, "INSERT INTO "+examUser+` VALUES
			('exam','equal',4,'1999-01-01',9), ('exam','below',4,'1999-01-01',9),
			('exam','zero',4,'1999-01-01',9), ('exam','negative',4,'1999-01-01',9),
			('exam','null',4,'1999-01-01',9), ('exam','rollback',4,'1999-01-01',9),
			('exam','overflow',94,'1999-01-01',9), ('other','equal',90,'1999-01-01',8),
			('exam','unanswered',5,'1999-01-01',9), ('exam','subjective',6,'1999-01-01',9),
			('no-config','equal',5,'1999-01-01',9)`)
		for _, user := range []string{"equal", "below", "zero", "negative", "null", "rollback", "overflow", "absent"} {
			businessExec(t, ctx, db, "INSERT INTO "+result+` VALUES
				('exam',$1,'single','prefix-single-guid-suffix'), ('exam',$1,'multi','wrong;multi-1;multi-2'),
				('exam',$1,'judge','yes'), ('exam',$1,'partial','one'),
				('exam',$1,'subjective','subjective-guid'), ('other',$1,'single','single-guid')`, user)
		}
		businessExec(t, ctx, db, "INSERT INTO "+result+` VALUES
			('exam','subjective','subjective','subjective-guid'), ('no-config','equal','single','single-guid')`)

		call := "CALL " + qualifiedFunction(schema, "automarkcount") + "($1::varchar,$2::varchar,$3::integer)"
		read := "SELECT \"CoDe\",\"ExamIsPass\",\"EndDate\" > TIMESTAMP '2000-01-01' FROM " + examUser +
			` WHERE "ExamGuid"=$1 AND "UserGuid"=$2`
		check := func(t *testing.T, row *sql.Row, wantCode, wantStatus int, wantEndDateChanged bool) {
			t.Helper()
			var code, status int
			var endDateChanged bool
			require.NoError(t, row.Scan(&code, &status, &endDateChanged))
			require.Equal(t, wantCode, code)
			require.Equal(t, wantStatus, status)
			require.Equal(t, wantEndDateChanged, endDateChanged)
		}
		for _, tc := range []struct {
			user       string
			passcode   any
			wantStatus int
			changeDate bool
		}{
			{"equal", 14, 1, true},
			{"below", 15, 0, true},
			{"zero", 0, 2, true},
			{"negative", -1, 9, false},
			{"null", nil, 9, false},
		} {
			t.Run(tc.user, func(t *testing.T) {
				businessExec(t, ctx, db, call, "exam", tc.user, tc.passcode)
				check(t, db.QueryRowContext(ctx, read, "exam", tc.user), 14, tc.wantStatus, tc.changeDate)
			})
		}
		check(t, db.QueryRowContext(ctx, read, "other", "equal"), 90, 8, false)
		for user, oldCode := range map[string]int{"unanswered": 5, "subjective": 6} {
			businessExec(t, ctx, db, call, "exam", user, 0)
			check(t, db.QueryRowContext(ctx, read, "exam", user), oldCode, 9, false)
		}
		businessExec(t, ctx, db, call, "exam", "absent", 0)

		_, err = db.ExecContext(ctx, call, "no-config", "equal", 0)
		require.ErrorContains(t, err, "examlist scoring record")
		check(t, db.QueryRowContext(ctx, read, "no-config", "equal"), 5, 9, false)
		_, err = db.ExecContext(ctx, call, "exam", "overflow", 0)
		require.Error(t, err)
		check(t, db.QueryRowContext(ctx, read, "exam", "overflow"), 94, 9, false)

		withBusinessCatalogPath(t, ctx, db, func(tx *sql.Tx) {
			_, err := tx.ExecContext(ctx, call, "exam", "rollback", 0)
			require.NoError(t, err)
			check(t, tx.QueryRowContext(ctx, read, "exam", "rollback"), 14, 2, true)
		})
		check(t, db.QueryRowContext(ctx, read, "exam", "rollback"), 4, 9, false)
	})
}
