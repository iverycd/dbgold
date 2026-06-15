// cmd/e2e_dameng/main.go
//
// MySQL → 达梦 端到端迁移验证程序(独立运行,不经 web 服务)。
// 直接调用 source.NewMySQL + target.NewDaMeng + datamigrate.NewMigrator,
// 跑完整三阶段迁移并打印 report,重点检查 IDENTITY/行数一致等实测项。
//
// 凭据从环境变量读取(密码不进命令行/会话记录):
//
//	MYSQL_HOST MYSQL_PORT MYSQL_DB MYSQL_USER MYSQL_PASS
//	DM_HOST    DM_PORT    DM_USER  DM_PASS    DM_SCHEMA
//
// 运行:
//
//	go run ./cmd/e2e_dameng
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"dbgold/datamigrate"
	"dbgold/datamigrate/source"
	"dbgold/datamigrate/target"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	mysqlHost := env("MYSQL_HOST", "192.168.149.92")
	mysqlPort := env("MYSQL_PORT", "3306")
	mysqlDB := env("MYSQL_DB", "epointbid_jingjia")
	mysqlUser := env("MYSQL_USER", "")
	mysqlPass := env("MYSQL_PASS", "")

	dmHost := env("DM_HOST", "192.168.149.92")
	dmPort := env("DM_PORT", "5236")
	dmUser := env("DM_USER", "")
	dmPass := env("DM_PASS", "")
	dmSchema := env("DM_SCHEMA", "test")

	if mysqlUser == "" || mysqlPass == "" || dmUser == "" || dmPass == "" {
		fmt.Println("缺少凭据,请设置环境变量: MYSQL_USER MYSQL_PASS DM_USER DM_PASS")
		os.Exit(1)
	}

	mysqlDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4",
		mysqlUser, mysqlPass, mysqlHost, mysqlPort, mysqlDB)
	dmDSN := fmt.Sprintf("dm://%s:%s@%s:%s", dmUser, dmPass, dmHost, dmPort)

	srcPool := source.ConnPoolConfig{}
	dstPool := target.ConnPoolConfig{}

	fmt.Printf("连接 MySQL %s:%s/%s ...\n", mysqlHost, mysqlPort, mysqlDB)
	reader, err := source.NewMySQL(mysqlDSN, mysqlDB, srcPool)
	if err != nil {
		fmt.Printf("[FATAL] 连接源库失败: %v\n", err)
		os.Exit(1)
	}
	defer reader.Close()

	fmt.Printf("连接 达梦 %s:%s schema=%s ...\n", dmHost, dmPort, dmSchema)
	writer, err := target.NewDaMeng(dmDSN, dmSchema, dstPool)
	if err != nil {
		fmt.Printf("[FATAL] 连接目标库失败: %v\n", err)
		os.Exit(1)
	}
	defer writer.Close()

	ctx := context.Background()

	// 校验目标 schema 是否存在
	if ok, err := writer.SchemaExists(ctx, dmSchema); err != nil {
		fmt.Printf("[FATAL] 检查目标 schema 失败: %v\n", err)
		os.Exit(1)
	} else if !ok {
		fmt.Printf("[FATAL] 目标 schema '%s' 不存在,请先在达梦中创建该用户/schema\n", dmSchema)
		os.Exit(1)
	}

	// 日志输出 goroutine
	job := &datamigrate.Job{
		LogCh:  make(chan string, 1024),
		Cancel: func() {},
	}
	done := make(chan struct{})
	go func() {
		for msg := range job.LogCh {
			fmt.Println(msg)
		}
		close(done)
	}()

	cfg := datamigrate.Config{
		PageSize:           20000,
		MaxParallel:        8,
		IntraTableParallel: 1,
		Mode:               "all",
		Filter:             "",
		Content:            "both",
		LowerCaseNames:     true, // 达梦默认大写,转小写 + 双引号保留
		CharInLength:       true, // 达梦 VARCHAR2 默认字节单位,中文需 CHAR 单位避免超长
		TargetSchema:       dmSchema,
		ChangeOwner:        false, // 达梦 ChangeOwner 是 no-op
		TargetDBType:       "dameng",
	}

	fmt.Println("=== 开始迁移 ===")
	start := time.Now()
	m := datamigrate.NewMigrator(reader, writer, job, cfg)
	report := m.Run(ctx)
	close(job.LogCh)
	<-done

	fmt.Println("\n=== 迁移报告 ===")
	printCat := func(name string, total, success, failed int) {
		fmt.Printf("  %-8s 总数=%d 成功=%d 失败=%d\n", name, total, success, failed)
	}
	printCat("表", report.Tables.Total, report.Tables.Success, report.Tables.Failed)
	printCat("数据", report.Data.Total, report.Data.Success, report.Data.Failed)
	printCat("主键", report.PrimaryKeys.Total, report.PrimaryKeys.Success, report.PrimaryKeys.Failed)
	printCat("索引", report.Indexes.Total, report.Indexes.Success, report.Indexes.Failed)
	printCat("外键", report.Constraints.Total, report.Constraints.Success, report.Constraints.Failed)
	printCat("序列", report.Sequences.Total, report.Sequences.Success, report.Sequences.Failed)
	printCat("视图", report.Views.Total, report.Views.Success, report.Views.Failed)
	fmt.Printf("  触发器  源库总数=%d (达梦目标不迁移触发器)\n", report.Triggers.Total)

	// 失败明细
	printFails := func(name string, items []datamigrate.ObjectResult) {
		for _, it := range items {
			fmt.Printf("  [FAIL][%s] %s: %s\n", name, it.Name, it.Error)
		}
	}
	printFails("表", report.Tables.Items)
	printFails("数据", report.Data.Items)
	printFails("主键", report.PrimaryKeys.Items)
	printFails("索引", report.Indexes.Items)
	printFails("外键", report.Constraints.Items)
	printFails("序列", report.Sequences.Items)
	printFails("视图", report.Views.Items)

	// 行数一致性
	fmt.Println("\n=== 行数对比(源 vs 目标)===")
	mismatch := 0
	for _, rc := range report.RowCounts {
		flag := "OK"
		if !rc.Match {
			flag = "MISMATCH"
			mismatch++
		}
		if !rc.Match || os.Getenv("VERBOSE") != "" {
			fmt.Printf("  [%s] %s 源=%d 目标=%d\n", flag, rc.Table, rc.Src, rc.Dst)
		}
	}

	totalFailed := report.Tables.Failed + report.Data.Failed + report.PrimaryKeys.Failed +
		report.Indexes.Failed + report.Constraints.Failed + report.Sequences.Failed + report.Views.Failed

	fmt.Printf("\n=== 总结 ===\n耗时 %s, 失败对象 %d, 行数不一致表 %d\n",
		time.Since(start).Round(time.Second), totalFailed, mismatch)
	if totalFailed > 0 || mismatch > 0 {
		os.Exit(2)
	}
}

// 防止 strconv 在未来未使用时报错(端口当前以字符串拼接,保留以备扩展)
var _ = strconv.Itoa
