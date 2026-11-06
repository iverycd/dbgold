-- SQL Server 2012 -> Kingbase，目标 schema: epointwjsj
-- 2026-11-06：使用当前 prepareView 与目标方言生成的两个可自动适配视图。
-- 基础表/相关数据恢复后，再在现有任务对象修复面板保存并执行对应视图草稿。

CREATE OR REPLACE VIEW "epointwjsj"."view_frame_user_secondou" AS SELECT * FROM (
SELECT "userguid",
         "loginid",
         "password",
         "ouguid",
         "displayname",
         "isenabled",
         "title",
         "leaderguid",
         "ordernumber",
         "telephoneoffice",
         "mobile",
         "email",
         "description",
         "telephonehome",
         "fax",
         "allowuseemail",
         "sex",
         "oucodelevel",
         "updatetime",
         "row_id"
    FROM "epointwjsj"."frame_user"
  union
  SELECT "a"."userguid",
         "loginid",
         "password",
         "a"."ouguid",
         "displayname",
         "isenabled",
         "a"."title",
         "leaderguid",
         "a"."userordernumber" as "ordernumber",
         "a"."tel" as "telephoneoffice",
         "mobile",
         "email",
         "description",
         "telephonehome",
         "fax",
         "allowuseemail",
         "sex",
         "oucodelevel",
         "updatetime",
         (1000000000 + "a"."row_id")   as "row_id"
    FROM "epointwjsj"."frame_user_secondou" "a"
    Left Join "epointwjsj"."frame_user" "b"
      on ("a"."userguid" = "b"."userguid")
) AS "__dbgold_view_columns" ("userguid", "loginid", "password", "ouguid", "displayname", "isenabled", "title", "leaderguid", "ordernumber", "telephoneoffice", "mobile", "email", "description", "telephonehome", "fax", "allowuseemail", "sex", "oucodelevel", "updatetime", "row_id");

CREATE OR REPLACE VIEW "epointwjsj"."view_portal_myitem" AS SELECT     "portal_myitem"."rowguid" AS "myrowguid", "portal_myitem"."userguid", "portal_item"."initrow", "portal_item"."initcol", "portal_item"."rowspan", "portal_item"."colspan", 
                      "portal_item"."belongportalguid", "portal_item"."title", "portal_item"."rowguid", "portal_myitem"."itemcol", "portal_myitem"."itemrow", "portal_myitem"."rowspan" AS "itemrowspan", 
                      "portal_myitem"."colspan" AS "itemcolspan", "portal_myitem"."disabled", "portal_item"."url"
FROM         "epointwjsj"."portal_item" INNER JOIN
                      "epointwjsj"."portal_myitem" ON ("portal_item"."rowguid" = "portal_myitem"."portaletguid")  
WHERE     ((CAST(CASE WHEN btrim("portal_item"."disabled") = '' THEN '0' ELSE "portal_item"."disabled" END AS integer) = 0));

