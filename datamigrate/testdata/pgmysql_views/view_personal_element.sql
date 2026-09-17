SELECT b.elementname, b.elementurl, b.rowguid AS elementguid,
    b.isdisable AS elementdisable, a.ptrowguid, a.isdisable, a.userguid,
    c.elementlocation AS initlocation, a.elementlocation AS userlocation,
    c.belongportalguid
   FROM njjwgch_djg_basedata_4_0_kf.personal_portal_element AS a
  CROSS JOIN njjwgch_djg_basedata_4_0_kf.app_element AS b
   JOIN njjwgch_djg_basedata_4_0_kf.app_portal_element AS c ON c.elementguid::text = b.rowguid::text AND a.ptrowguid::text = c.rowguid::text
