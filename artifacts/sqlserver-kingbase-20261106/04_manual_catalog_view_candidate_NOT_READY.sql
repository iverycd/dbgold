-- 人工候选：禁止直接执行。
-- 尚未验证列名作为业务值的语义，以及 table_struct.fieldName 的大小写、排序规则和目标名称映射。
-- 该候选只替换已识别的 syscolumns/OBJECT_ID 列名查询，其余源语法仍须逐项适配。
-- 必须人工核对、编辑、验证并保存为新草稿后，才能从对象修复面板执行。

CREATE OR REPLACE VIEW "epointwjsj"."view_tjfx_hzzdnameandtable" AS
-- 仅供人工适配：请确认 name 返回源列名还是目标列名，并核对 table_struct.fieldName 的大小写/排序规则。
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
				SELECT c.column_name AS name FROM information_schema.columns c WHERE c.table_schema = 'epointwjsj' AND c.table_name = 'view_tjfx_infohz'
					
				
					
				
					  
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
LEFT JOIN table_basicinfo f ON f.tableid = h.tableid;
