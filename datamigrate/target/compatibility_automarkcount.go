package target

import "strings"

func autoMarkCountDDL(schema, catalog string, native bool) string {
	return autoMarkCountDDLWithDate(schema, catalog, native, `{{schema}}."getdate"()`)
}

func autoMarkCountDDLWithDate(schema, catalog string, native bool, dateExpression string) string {
	tables := []pgProcedureTable{
		{"examuser", "examuserlist", strings.Fields("examguid userguid code enddate examispass")},
		{"exam", "examlist", strings.Fields("examguid singlescore multiscore judgescore")},
		{"question", "examquestionlist", strings.Fields("examguid subjectguid ischeck")},
		{"result", "examresultlist", strings.Fields("examguid userguid subjectguid answerid")},
		{"answer", "examanswer", strings.Fields("subjectguid isright answerguid")},
	}
	const userMatch = ` WHERE u.{{examuser.examguid}}=$1 AND u.{{examuser.userguid}}=$2`
	const updateScore = `UPDATE {{examuser}} AS u SET {{examuser.code}}=$3`
	completeScore := updateScore + `, {{examuser.enddate}}=` + dateExpression + `, {{examuser.examispass}}=`
	queries := map[string]string{
		"INITIAL_SCORE": `SELECT u.{{examuser.code}} FROM {{examuser}} AS u` + userMatch + ` LIMIT 1 FOR UPDATE`,
		"RESPONSES": `SELECT r.{{result.subjectguid}} AS subject_guid, r.{{result.answerid}} AS answer_id,
       q.{{question.ischeck}} AS question_type
FROM {{result}} AS r
JOIN {{question}} AS q ON q.{{question.examguid}}=r.{{result.examguid}}
 AND q.{{question.subjectguid}}=r.{{result.subjectguid}}
WHERE q.{{question.examguid}}=$1 AND r.{{result.userguid}}=$2
 AND q.{{question.ischeck}} IN ('0','1','2')`,
		"QUESTION_SCORE": `SELECT CASE $2
 WHEN '0' THEN e.{{exam.singlescore}}::integer
 WHEN '1' THEN e.{{exam.multiscore}}::integer
 WHEN '2' THEN e.{{exam.judgescore}}::integer
END
FROM {{exam}} AS e WHERE e.{{exam.examguid}}=$1`,
		"ANSWER_COUNTS": `SELECT {{catalog}}.count(*),
 {{catalog}}.count(CASE WHEN {{catalog}}.strpos($2::text,a.{{answer.answerguid}}::text)>0 THEN 1 END)
FROM {{answer}} AS a WHERE a.{{answer.subjectguid}}=$1 AND a.{{answer.isright}}='1'`,
		"MARK_NO_THRESHOLD": completeScore + "'2'" + userMatch,
		"MARK_FAILED":       completeScore + "'0'" + userMatch,
		"MARK_PASSED":       completeScore + "'1'" + userMatch,
		"SAVE_SCORE":        updateScore + userMatch,
	}
	return pgBusinessProcedureDDL(schema, catalog, native, "automarkcount",
		"IN p_examguid varchar, IN p_userguid varchar, IN p_passcode integer",
		tables, "", autoMarkCountDeclarations, autoMarkCountStatements, queries)
}

const autoMarkCountDeclarations = ` accumulated_score integer;
 question_score integer;
 right_count bigint;
 matched_count bigint;
 response_row record;`

const autoMarkCountStatements = ` BEGIN
  EXECUTE {{INITIAL_SCORE}} INTO STRICT accumulated_score USING p_examguid,p_userguid;
 EXCEPTION WHEN NO_DATA_FOUND THEN
  RETURN;
 END;

 FOR response_row IN EXECUTE {{RESPONSES}} USING p_examguid,p_userguid LOOP
  BEGIN
   EXECUTE {{QUESTION_SCORE}} INTO STRICT question_score USING p_examguid,response_row.question_type;
  EXCEPTION WHEN NO_DATA_FOUND OR TOO_MANY_ROWS THEN
   RAISE EXCEPTION 'automarkcount: expected one examlist scoring record for exam %',p_examguid;
  END;
  IF question_score IS NULL THEN
   RAISE EXCEPTION 'automarkcount: examlist score is missing for exam %, question type %',
    p_examguid,response_row.question_type;
  END IF;

  -- Count every correct answer whose GUID occurs in the supplied answer.
  -- Extra incorrect answers do not alter this count.
  EXECUTE {{ANSWER_COUNTS}} INTO right_count,matched_count
   USING response_row.subject_guid,response_row.answer_id;
  IF right_count=matched_count THEN
   accumulated_score := accumulated_score+question_score;
  END IF;

  IF p_passcode=0 THEN
   EXECUTE {{MARK_NO_THRESHOLD}} USING p_examguid,p_userguid,accumulated_score;
  ELSIF p_passcode>accumulated_score THEN
   EXECUTE {{MARK_FAILED}} USING p_examguid,p_userguid,accumulated_score;
  ELSIF accumulated_score>=p_passcode AND p_passcode>0 THEN
   EXECUTE {{MARK_PASSED}} USING p_examguid,p_userguid,accumulated_score;
  ELSE
   EXECUTE {{SAVE_SCORE}} USING p_examguid,p_userguid,accumulated_score;
  END IF;
 END LOOP;`
