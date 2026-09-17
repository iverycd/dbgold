SELECT COALESCE(memuserrelation.relateuserguid::text, memuser.userguid::text) AS "UserGuid",
    memuser.loginid AS "LoginID", memuser.password, memuser.ouguid AS "OUGuid",
    memuser.displayname AS "DisplayName", memuser.isenabled AS "IsEnabled",
    memuser.status, memuser.usertype AS "UserType",
    memuser.legalusertype AS "legalUserType", memuser.baseuserguid,
    memuser.ordernumber AS "OrderNumber",
    memuser.telephoneoffice AS "TelephoneOffice", memuser.mobile AS "Mobile",
    memuser.email AS "Email", memuser.description AS "Description",
    memuser.telephonehome AS "TelephoneHome", memuser.fax AS "Fax",
    memuser.enterpriseguid AS "enterpriseGuid", memuser.sex AS "Sex",
    memuser.updatetime AS "UpDateTime", memuserextendinfo.birthday,
    memuserextendinfo.shortmobile AS "Shortmobile",
    memuserextendinfo.usbkey AS "Usbkey",
    memuserextendinfo.qqnumber AS "Qqnumber",
    memuserextendinfo.msnnumber AS "Msnnumber",
    memuserextendinfo.postaladdress AS "Postaladdress",
    memuserextendinfo.postalcode AS "Postalcode",
    memuserextendinfo.cardtype AS "CardType",
    memuserextendinfo.cardnum AS "cardNum", memuserextendinfo.carnum,
    memou.ouname AS "Ouname", memou.oushortname AS "Oushortname",
    memou.parentouguid AS "Parentouguid", memou.oucodelevel AS "Oucodelevel",
    memou.oucodefull AS "OucodeFull"
   FROM njjwgch_djg_basedata_4_0_kf.mem_user AS memuser
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.mem_ou AS memou ON memuser.ouguid::text = memou.ouguid::text
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.mem_user_extendinfo AS memuserextendinfo ON memuser.userguid::text = memuserextendinfo.userguid::text
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.mem_user_relation AS memuserrelation ON memuser.userguid::text = memuserrelation.userguid::text AND memuserrelation.relatetype::text = 'L'::text AND memuser.usertype::text = 'O'::text
