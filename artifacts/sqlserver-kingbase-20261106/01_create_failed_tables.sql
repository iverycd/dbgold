-- SQL Server 2012 -> Kingbase，目标 schema: epointwjsj
-- 2026-11-06：当前代码根据源库只读列元数据生成，仅供创建这两张缺失表。
-- 执行前逐表核验目标同名表不存在；若已存在，请在面板检查结构，勿重复创建。
-- 本文件只包含 CREATE TABLE；不含数据、主键、索引、序列或清空语句。

CREATE TABLE "epointwjsj"."frame_myaddressgroup_snapshot" (
  "groupguid" varchar(100) NOT NULL,
  "groupname" varchar(100) DEFAULT NULL,
  "owneruserguid" varchar(100) DEFAULT NULL,
  "belongbaseouguid" varchar(100) DEFAULT NULL,
  "ordernumber" int DEFAULT '0',
  "clientip" varchar(50) DEFAULT NULL,
  "appkey" varchar(100) DEFAULT NULL,
  "parentgroupguid" varchar(100) DEFAULT NULL,
  "rowguid" varchar(50) NOT NULL,
  "appguid" varchar(50),
  "isfilter" varchar(4)
);

CREATE TABLE "epointwjsj"."sj_xxgl_case" (
  "belongxiaqucode" varchar(50),
  "operateusername" varchar(50),
  "operatedate" date,
  "yearflag" varchar(4),
  "rowguid" varchar(50) NOT NULL,
  "caseguid" varchar(50),
  "casetypecd" varchar(25),
  "casename" varchar(100),
  "pricecollecttime" date,
  "totalprice" varchar(50),
  "casedesc" varchar(100),
  "createtime" date,
  "validind" varchar(2),
  "clicknum" decimal(11,0),
  "createuserguid" varchar(50),
  "regioncd" varchar(50),
  "regionname" varchar(50),
  "xznum" decimal(11,0) DEFAULT '0',
  "je" decimal(11,0) DEFAULT '0'
);

