SELECT portal_myitem.rowguid AS myrowguid, portal_myitem.userguid,
    portal_item.initrow, portal_item.initcol, portal_item.rowspan,
    portal_item.colspan, portal_item.belongportalguid, portal_item.title,
    portal_item.rowguid, portal_myitem.itemcol, portal_myitem.itemrow,
    portal_myitem.rowspan AS itemrowspan, portal_myitem.colspan AS itemcolspan,
    portal_myitem.disabled, portal_item.url
   FROM njjwgch_djg_basedata_4_0_kf.portal_item
   JOIN njjwgch_djg_basedata_4_0_kf.portal_myitem ON portal_item.rowguid::text = portal_myitem.portaletguid::text
  WHERE portal_item.disabled = B'0'::"bit"
