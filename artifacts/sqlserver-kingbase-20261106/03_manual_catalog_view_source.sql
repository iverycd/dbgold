-- SQL Server 源库原始视图 DDL；正文原样保留，仅供人工比对。
-- 此文件不是 Kingbase 目标 DDL，不可直接在目标库执行。

CREATE VIEW [dbo].[view_tjfx_hzzdnameandtable] AS 
SELECT
	h.name,
	h.fieldchinesename,
	f.sql_tablename,
  f.ordernum
FROM
	(
		SELECT
			t.name,
			q.fieldchinesename,
			q.tableid
		FROM
			(
				SELECT
					*
				FROM
					syscolumns
				WHERE
					id = object_id('view_tjfx_infohz')
			) t
		LEFT JOIN (
			SELECT
				*
			FROM
				Table_Struct t
			WHERE
				t.TableID IN (
					SELECT
						tb.tableid
					FROM
						table_basicinfo tb
					WHERE
						LOWER (tb.sql_tablename) IN (
							'sj_project',
							'sj_xmss',
							'sj_xmfw',
							'sj_fpdj',
							'sj_otherwt',
							'sj_otherquestion',
							'sj_questype',
							'sj_fenpei',
							'sj_pro_fenpei',
							'sj_plan_main',
							'sj_pro_plan'
						)
				)
		) q ON q.fieldname = t.name
		GROUP BY
			t.name,
			q.fieldchinesename,
			q.tableid
	) h
LEFT JOIN table_basicinfo f ON f.tableid = h.tableid
