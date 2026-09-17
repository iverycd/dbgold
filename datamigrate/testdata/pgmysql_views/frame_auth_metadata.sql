SELECT o.rowguid AS operationguid, r.rowguid AS resourceguid,
    e.appguid AS "appGuid", e.rowguid AS expressionguid, e.expressiontype,
    er.datainsguid, e.rightlevel, e.expressionname, o.operationname,
    r.resourcetype, r.rangetype, r.resourcerange, r.expression
   FROM njjwgch_djg_basedata_4_0_kf.frame_authority_expression AS e
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.frame_auth_exp_resource AS er ON er.expressionguid::text = e.rowguid::text
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.frame_auth_opt AS o ON o.rowguid::text = er.operationguid::text
   LEFT JOIN njjwgch_djg_basedata_4_0_kf.frame_auth_opt_resource AS r ON r.operationguid::text = o.rowguid::text
