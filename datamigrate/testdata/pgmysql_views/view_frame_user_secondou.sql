SELECT frame_user.userguid AS "UserGuid",
    frame_user.loginid AS "LoginID", frame_user.password,
    frame_user.ouguid AS "OUGuid",
    frame_user.displayname AS "DisplayName",
    frame_user.isenabled AS "IsEnabled", frame_user.title AS "Title",
    frame_user.leaderguid AS "LeaderGuid",
    frame_user.ordernumber AS "OrderNumber",
    frame_user.telephoneoffice AS "TelephoneOffice",
    frame_user.mobile AS "Mobile", frame_user.email AS "Email",
    frame_user.description AS "Description",
    frame_user.telephonehome AS "TelephoneHome",
    frame_user.fax AS "Fax",
    frame_user.allowuseemail AS "AllowUseEmail",
    frame_user.sex AS "Sex", frame_user.oucodelevel AS "OUCodeLevel",
    frame_user.updatetime AS "UpDateTime",
    frame_user.row_id AS "Row_id"
   FROM njjwgch_djg_basedata_4_0_kf.frame_user
UNION
 SELECT a.userguid AS "UserGuid", b.loginid AS "LoginID", b.password,
    a.ouguid AS "OUGuid", b.displayname AS "DisplayName",
    b.isenabled AS "IsEnabled", a.title AS "Title",
    b.leaderguid AS "LeaderGuid", a.userordernumber AS "OrderNumber",
    a.tel AS "TelephoneOffice", b.mobile AS "Mobile",
    b.email AS "Email", b.description AS "Description",
    b.telephonehome AS "TelephoneHome", b.fax AS "Fax",
    b.allowuseemail AS "AllowUseEmail", b.sex AS "Sex",
    b.oucodelevel AS "OUCodeLevel", b.updatetime AS "UpDateTime",
    1000000000 + a.row_id AS "Row_id"
   FROM njjwgch_djg_basedata_4_0_kf.frame_user_secondou AS a
  LEFT JOIN njjwgch_djg_basedata_4_0_kf.frame_user AS b ON a.userguid::text = b.userguid::text
