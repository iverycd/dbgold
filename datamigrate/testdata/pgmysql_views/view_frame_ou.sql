SELECT frame_ou.ouguid AS "Ouguid", frame_ou.oucode AS "Oucode",
    frame_ou.ouname AS "Ouname", frame_ou.oushortname AS "Oushortname",
    frame_ou.ordernumber AS "Ordernumber",
    frame_ou.description AS "Description", frame_ou.address AS "Address",
    frame_ou.postalcode AS "Postalcode", frame_ou.tel AS "Tel",
    frame_ou.baseouguid AS "Baseouguid",
    frame_ou.issubwebflow AS "Issubwebflow",
    frame_ou.parentouguid AS "Parentouguid",
    frame_ou.oucodelevel AS "Oucodelevel", frame_ou.updatetime AS "Updatetime",
    frame_ou.haschildou AS "Haschildou",
    frame_ou.haschilduser AS "Haschilduser",
    frame_ou_extendinfo.isindependence AS "Isindependence"
   FROM njjwgch_djg_basedata_4_0_kf.frame_ou
  CROSS JOIN njjwgch_djg_basedata_4_0_kf.frame_ou_extendinfo
  WHERE frame_ou.ouguid::text = frame_ou_extendinfo.ouguid::text
