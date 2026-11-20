CREATE VIEW view_hetong_yuqitongji_ysjx AS
SELECT
  cg_hetongtiaokuan.Row_ID AS row_id,
  IFNULL(cg_hetongtiaokuan.islvyue, 0) AS islvyue,
  (SELECT view_cg_hetongandbiangeng.HTNO FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = cg_hetongtiaokuan.beianguid) AS htno,
  (SELECT view_cg_hetongandbiangeng.HTNAME FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = cg_hetongtiaokuan.beianguid) AS htname,
  (SELECT view_cg_hetongandbiangeng.JIANSHEDW FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = cg_hetongtiaokuan.beianguid) AS jianshedw,
  (SELECT view_cg_hetongandbiangeng.ZBDANWEINAME FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = cg_hetongtiaokuan.beianguid) AS zbdanweiname,
  (SELECT view_cg_hetongandbiangeng.ZBDANWEIGUID FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = cg_hetongtiaokuan.beianguid) AS zbdanweiguid,
  (TO_DAYS(NOW()) - TO_DAYS(cg_hetongtiaokuan.FuKuanTime)) AS yuqidaycount,
  cg_hetongtiaokuan.FuKuanTime AS FuKuanTime,
  NULL AS realFuKuanTime,
  cg_hetongtiaokuan.FuKuanNode AS fukuannode,
  cg_hetongtiaokuan.FuKuanPrice AS fukuanprice,
  cg_hetongtiaokuan.FuKuanContent AS FuKuanContent,
  cg_hetongtiaokuan.RowGuid AS rowguid,
  cg_hetongtiaokuan.SBR_UnitGuid AS SBR_UnitGuid,
  cg_hetongtiaokuan.tenantGuid AS tenantGuid
FROM cg_hetongtiaokuan
WHERE cg_hetongtiaokuan.beianguid IN (
  SELECT cg_hetonginfo.BeiAnGuid FROM cg_hetonginfo
  WHERE IFNULL(cg_hetonginfo.isbiangeng, '') <> '1'
    AND IFNULL(cg_hetonginfo.isterminate, '') <> '1' AND cg_hetonginfo.AuditStatus = '3'
  UNION ALL
  SELECT cg_hetongbiangeng.BeiAnGuid FROM cg_hetongbiangeng
  WHERE IFNULL(cg_hetongbiangeng.isbiangeng, '') <> '1'
    AND IFNULL(cg_hetongbiangeng.Isterminate, '') <> '1' AND cg_hetongbiangeng.AuditStatus = '3'
)
AND IFNULL(cg_hetongtiaokuan.FuKuanTime,'') <> ''
AND cg_hetongtiaokuan.FuKuanTime < (NOW() + INTERVAL 7 DAY)
AND NOT (cg_hetongtiaokuan.RowGuid IN (
  SELECT cg_paymentvoucher.tiaokuanguid FROM cg_paymentvoucher WHERE cg_paymentvoucher.AuditStatus = '3'
))
UNION ALL
SELECT
  a.Row_ID AS row_id,
  IFNULL(a.islvyue, 0) AS islvyue,
  (SELECT view_cg_hetongandbiangeng.HTNO FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = a.beianguid) AS htno,
  (SELECT view_cg_hetongandbiangeng.HTNAME FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = a.beianguid) AS htname,
  (SELECT view_cg_hetongandbiangeng.JIANSHEDW FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = a.beianguid) AS jianshedw,
  (SELECT view_cg_hetongandbiangeng.ZBDANWEINAME FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = a.beianguid) AS zbdanweiname,
  (SELECT view_cg_hetongandbiangeng.ZBDANWEIGUID FROM view_cg_hetongandbiangeng
     WHERE view_cg_hetongandbiangeng.BEIANGUID = a.beianguid) AS zbdanweiguid,
  (TO_DAYS(b.SHR_Date) - TO_DAYS(a.FuKuanTime)) AS yuqidaycount,
  a.FuKuanTime AS FuKuanTime,
  b.SHR_Date AS realFuKuanTime,
  a.FuKuanNode AS fukuannode,
  a.FuKuanPrice AS fukuanprice,
  a.FuKuanContent AS FuKuanContent,
  a.RowGuid AS rowguid,
  a.SBR_UnitGuid AS SBR_UnitGuid,
  a.tenantGuid AS tenantGuid
FROM (cg_hetongtiaokuan a JOIN cg_paymentvoucher b ON ((a.RowGuid = b.tiaokuanguid)))
WHERE a.beianguid IN (
  SELECT cg_hetonginfo.BeiAnGuid FROM cg_hetonginfo
  WHERE IFNULL(cg_hetonginfo.isbiangeng, '') <> '1'
    AND IFNULL(cg_hetonginfo.isterminate, '') <> '1' AND cg_hetonginfo.AuditStatus = '3'
  UNION ALL
  SELECT cg_hetongbiangeng.BeiAnGuid FROM cg_hetongbiangeng
  WHERE IFNULL(cg_hetongbiangeng.isbiangeng, '') <> '1'
    AND IFNULL(cg_hetongbiangeng.Isterminate, '') <> '1' AND cg_hetongbiangeng.AuditStatus = '3'
)
AND IFNULL(a.FuKuanTime,'') <> ''
AND b.AuditStatus = '3' AND a.FuKuanTime < b.SHR_Date
