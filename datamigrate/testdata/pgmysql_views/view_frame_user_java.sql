SELECT DISTINCT frame_user.userguid, frame_user.loginid, frame_user.password,
    frame_user.ouguid, frame_user.displayname, frame_user.isenabled,
    frame_user.title, frame_user.leaderguid, frame_user.ordernumber,
    frame_user.telephoneoffice, frame_user.mobile, frame_user.email,
    frame_user.description, frame_user.telephonehome, frame_user.fax,
    frame_user.allowuseemail, frame_user.sex, frame_user.updatetime,
    frame_user.issyncthirdparty, frame_user_extendinfo.birthday,
    frame_user_extendinfo.shortmobile, frame_user_extendinfo.usbkey,
    frame_user_extendinfo.ntx_extnumber, frame_user_extendinfo.qqnumber,
    frame_user_extendinfo.msnnumber, frame_user_extendinfo.postaladdress,
    frame_user_extendinfo.postalcode, frame_user_extendinfo.carnum,
    frame_ou.ouname, frame_ou.oushortname, frame_ou.parentouguid,
    frame_ou.oucodelevel
   FROM njjwgch_djg_basedata_4_0_kf.frame_user
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.frame_ou ON frame_user.ouguid::text = frame_ou.ouguid::text
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.frame_user_extendinfo ON frame_user.userguid::text = frame_user_extendinfo.userguid::text
