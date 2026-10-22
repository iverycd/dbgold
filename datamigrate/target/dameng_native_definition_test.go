package target

import "testing"

func TestNativeDefinitionNormalizesOnlyEquivalentUppercaseIdentifiers(t *testing.T) {
	if !nativeDefinitionsEqual(`CREATE TABLE "SCHEMA_UPPER"."TABLE_UPPER"("ID" INT)`, `CREATE TABLE SCHEMA_UPPER.TABLE_UPPER(ID INT)`) {
		t.Fatal("GET_DDL 的未加引号大写标识符与精确大写引号标识符应相等")
	}
	if nativeDefinitionsEqual(`CREATE TABLE "MiX"."T"("Id" INT)`, `CREATE TABLE MIX.T(ID INT)`) {
		t.Fatal("混合大小写标识符不能折叠")
	}
	if nativeDefinitionsEqual(`CREATE TABLE S.T(C VARCHAR(20) DEFAULT 'MiX')`, `CREATE TABLE S.T(C VARCHAR(20) DEFAULT 'MIX')`) {
		t.Fatal("字符串内容不能折叠")
	}
}

func TestNativePartitionDefinitionAllowsOnlyIdenticalInheritedStorage(t *testing.T) {
	want := `CREATE TABLE "S"."Buffer"("Id" INT) PARTITION BY RANGE("Id") (PARTITION "P" VALUES LESS THAN(MAXVALUE)) STORAGE(ON "MAIN",CLUSTERBTR)`
	got := `CREATE TABLE "S"."Buffer"("Id" INT) PARTITION BY RANGE("Id") (PARTITION "P" VALUES LESS THAN(MAXVALUE) STORAGE(ON "MAIN",CLUSTERBTR)) STORAGE(ON "MAIN",CLUSTERBTR)`
	if !nativeDefinitionsEqual(want, got) {
		t.Fatal("GET_DDL 显式继承的同一存储配置应等价")
	}
	wrong := `CREATE TABLE "S"."Buffer"("Id" INT) PARTITION BY RANGE("Id") (PARTITION "P" VALUES LESS THAN(MAXVALUE) STORAGE(ON "OTHER",CLUSTERBTR)) STORAGE(ON "MAIN",CLUSTERBTR)`
	if nativeDefinitionsEqual(want, wrong) {
		t.Fatal("不能忽略分区不同的表空间")
	}
	wrong = `CREATE TABLE "S"."Buffer"("Id" INT) PARTITION BY RANGE("Id") (PARTITION "P" VALUES LESS THAN(10) STORAGE(ON "MAIN",CLUSTERBTR)) STORAGE(ON "MAIN",CLUSTERBTR)`
	if nativeDefinitionsEqual(want, wrong) {
		t.Fatal("不能忽略分区边界")
	}
}
