// api/handler/batchmigration.go
package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dbgold/datamigrate"
	"dbgold/middleware"
	"dbgold/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// dbTypeAlias 把 Excel 中常见的库类型写法归一化为系统内部类型名。
// 例如示例模板里目标库写的是 "gauss"，系统类型是 "gaussdb"。
var dbTypeAlias = map[string]string{
	"mysql":      "mysql",
	"mariadb":    "mysql",
	"postgres":   "postgres",
	"postgresql": "postgres",
	"pg":         "postgres",
	"oracle":     "oracle",
	"sqlserver":  "sqlserver",
	"mssql":      "sqlserver",
	"gaussdb":    "gaussdb",
	"gauss":      "gaussdb",
	"opengauss":  "gaussdb",
	"dameng":     "dameng",
	"dm":         "dameng",
	"seabox":     "seabox",
	"highgo":     "highgo",
}

// normalizeDBType 去空格转小写后查别名表，查不到则原样返回（便于在 reason 中提示原值）。
func normalizeDBType(s string) string {
	k := strings.ToLower(strings.TrimSpace(s))
	if v, ok := dbTypeAlias[k]; ok {
		return v
	}
	return k
}

// batchRow 表示 Excel 中一行的解析与校验结果。出于安全，密码字段不参与 JSON 序列化。
type batchRow struct {
	RowNum int `json:"row_num"` // Excel 中的行号（含表头，从 2 起）

	SrcDBType   string `json:"src_db_type"`
	SrcHost     string `json:"src_host"`
	SrcPort     int    `json:"src_port"`
	SrcDatabase string `json:"src_database"`
	SrcUsername string `json:"src_username"`
	SrcPassword string `json:"-"`

	DstDBType    string `json:"dst_db_type"`
	DstHost      string `json:"dst_host"`
	DstPort      int    `json:"dst_port"`
	DstDatabase  string `json:"dst_database"`
	DstUsername  string `json:"dst_username"`
	DstPassword  string `json:"-"`
	TargetSchema string `json:"target_schema"`

	Supported bool   `json:"supported"`
	Reason    string `json:"reason"` // 不支持/缺字段的原因
}

type batchValidateResult struct {
	Rows             []batchRow `json:"rows"`
	SupportedCount   int        `json:"supported_count"`
	UnsupportedCount int        `json:"unsupported_count"`
}

// 模板表头（列名），解析时按列名定位，容忍列顺序变化。
var batchHeaders = []string{
	"source_db_type", "source_host", "source_port", "source_database", "source_username", "source_password",
	"target_db_type", "target_host", "target_port", "target_database", "target_username", "target_password",
	"target_schema",
}

// parseBatchExcel 从上传的 Excel 内容解析所有数据行并完成校验。
func parseBatchExcel(f *excelize.File) ([]batchRow, error) {
	sheet := "db_config"
	found := false
	for _, s := range f.GetSheetList() {
		if s == sheet {
			found = true
			break
		}
	}
	if !found {
		list := f.GetSheetList()
		if len(list) == 0 {
			return nil, fmt.Errorf("Excel 中没有工作表")
		}
		sheet = list[0]
	}

	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, fmt.Errorf("读取工作表失败: %v", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("Excel 没有数据行（至少需要表头 + 一行数据）")
	}

	// 按表头名定位列下标
	header := rows[0]
	colIdx := make(map[string]int)
	for i, h := range header {
		colIdx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, h := range batchHeaders {
		if _, ok := colIdx[h]; !ok {
			return nil, fmt.Errorf("缺少必需列: %s", h)
		}
	}

	cell := func(row []string, name string) string {
		i := colIdx[name]
		if i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}

	var result []batchRow
	for r := 1; r < len(rows); r++ {
		row := rows[r]
		// 整行空白则跳过
		if strings.TrimSpace(strings.Join(row, "")) == "" {
			continue
		}
		br := batchRow{
			RowNum:       r + 1,
			SrcDBType:    normalizeDBType(cell(row, "source_db_type")),
			SrcHost:      cell(row, "source_host"),
			SrcDatabase:  cell(row, "source_database"),
			SrcUsername:  cell(row, "source_username"),
			SrcPassword:  cell(row, "source_password"),
			DstDBType:    normalizeDBType(cell(row, "target_db_type")),
			DstHost:      cell(row, "target_host"),
			DstDatabase:  cell(row, "target_database"),
			DstUsername:  cell(row, "target_username"),
			DstPassword:  cell(row, "target_password"),
			TargetSchema: cell(row, "target_schema"),
		}
		validateBatchRow(&br, cell(row, "source_port"), cell(row, "target_port"))
		result = append(result, br)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Excel 没有有效数据行")
	}
	return result, nil
}

// validateBatchRow 完成单行的端口解析、必填校验与迁移对支持性校验，写入 Supported/Reason。
func validateBatchRow(br *batchRow, srcPortStr, dstPortStr string) {
	var missing []string

	if br.SrcDBType == "" {
		missing = append(missing, "source_db_type")
	}
	if br.SrcHost == "" {
		missing = append(missing, "source_host")
	}
	if p, err := strconv.Atoi(srcPortStr); err == nil && p > 0 {
		br.SrcPort = p
	} else {
		missing = append(missing, "source_port")
	}
	if br.SrcDatabase == "" {
		missing = append(missing, "source_database")
	}
	if br.SrcUsername == "" {
		missing = append(missing, "source_username")
	}

	if br.DstDBType == "" {
		missing = append(missing, "target_db_type")
	}
	if br.DstHost == "" {
		missing = append(missing, "target_host")
	}
	if p, err := strconv.Atoi(dstPortStr); err == nil && p > 0 {
		br.DstPort = p
	} else {
		missing = append(missing, "target_port")
	}
	if br.DstDatabase == "" {
		missing = append(missing, "target_database")
	}
	if br.DstUsername == "" {
		missing = append(missing, "target_username")
	}
	if br.TargetSchema == "" {
		missing = append(missing, "target_schema")
	}

	if len(missing) > 0 {
		br.Supported = false
		br.Reason = "缺少必填字段: " + strings.Join(missing, ", ")
		return
	}

	if !isSupportedPair(br.SrcDBType, br.DstDBType) {
		br.Supported = false
		br.Reason = fmt.Sprintf("不支持 %s → %s 的数据迁移", br.SrcDBType, br.DstDBType)
		return
	}
	br.Supported = true
}

// readUploadedExcel 从 multipart 表单字段 "file" 读取并打开 Excel。
func readUploadedExcel(c *gin.Context) (*excelize.File, string, error) {
	fh, err := c.FormFile("file")
	if err != nil {
		return nil, "", fmt.Errorf("请上传 Excel 文件")
	}
	src, err := fh.Open()
	if err != nil {
		return nil, "", fmt.Errorf("打开上传文件失败: %v", err)
	}
	defer src.Close()
	f, err := excelize.OpenReader(src)
	if err != nil {
		return nil, "", fmt.Errorf("解析 Excel 失败（请确认为 .xlsx 格式）: %v", err)
	}
	return f, fh.Filename, nil
}

// ValidateBatch 解析上传的 Excel 并返回每行的校验结果，不执行任何迁移。
func ValidateBatch(c *gin.Context) {
	f, _, err := readUploadedExcel(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer f.Close()

	rows, err := parseBatchExcel(f)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res := batchValidateResult{Rows: rows}
	for _, r := range rows {
		if r.Supported {
			res.SupportedCount++
		} else {
			res.UnsupportedCount++
		}
	}
	c.JSON(http.StatusOK, res)
}

// batchOptions 是整批共用的迁移选项，对齐单任务前端默认值（MigrationView.vue）。
// 这些选项应用到批次内所有迁移对。
type batchOptions struct {
	MigrateContent     string // both / schema_only / data_only
	PageSize           int
	MaxParallel        int
	IntraTableParallel int
	LowerCaseNames     bool
	CharInLength       bool
	UseNvarchar2       bool
	Distributed        bool
	ChangeOwner        bool
	StripViewSchemas   string
}

// parseBatchOptions 从 multipart 表单读取批次迁移选项；字段缺省时回落到单任务默认值。
func parseBatchOptions(c *gin.Context) batchOptions {
	// 布尔字段：表单缺省（空串）时用默认值，否则按 "true" 解析
	boolOr := func(key string, def bool) bool {
		v := c.PostForm(key)
		if v == "" {
			return def
		}
		return v == "true" || v == "1"
	}
	intOr := func(key string, def int) int {
		if n, err := strconv.Atoi(c.PostForm(key)); err == nil && n > 0 {
			return n
		}
		return def
	}
	content := c.PostForm("migrate_content")
	if content == "" {
		content = "both"
	}
	return batchOptions{
		MigrateContent:     content,
		PageSize:           intOr("page_size", 20000),
		MaxParallel:        intOr("max_parallel", 10),
		IntraTableParallel: intOr("intra_table_parallel", 8),
		LowerCaseNames:     boolOr("lower_case_names", true),
		CharInLength:       boolOr("char_in_length", false),
		UseNvarchar2:       boolOr("use_nvarchar2", false),
		Distributed:        boolOr("distributed", false),
		ChangeOwner:        boolOr("change_owner", true),
		StripViewSchemas:   c.PostForm("strip_view_schemas"),
	}
}

// StartBatch 重新解析上传的 Excel，过滤掉不支持行与被排除行号，创建批次并启动串行 worker 执行。
func StartBatch(c *gin.Context) {
	f, fileName, err := readUploadedExcel(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer f.Close()

	rows, err := parseBatchExcel(f)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 解析被用户排除的行号
	excluded := make(map[int]bool)
	if s := c.PostForm("exclude_rows"); s != "" {
		for _, part := range strings.Split(s, ",") {
			if n, e := strconv.Atoi(strings.TrimSpace(part)); e == nil {
				excluded[n] = true
			}
		}
	}

	var valid []batchRow
	for _, r := range rows {
		if r.Supported && !excluded[r.RowNum] {
			valid = append(valid, r)
		}
	}
	if len(valid) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可执行的迁移对（受支持且未被排除）"})
		return
	}

	ownerID := middleware.GetCurrentUserID(c)
	batchID := uuid.New().String()
	batch := &store.BatchMigration{
		OwnerID:  ownerID,
		BatchID:  batchID,
		FileName: fileName,
		Total:    len(valid),
		Status:   "running",
	}
	if err := store.CreateBatchMigration(batch); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建批次记录失败"})
		return
	}

	opts := parseBatchOptions(c)
	go runBatch(batchID, ownerID, valid, opts)

	c.JSON(http.StatusOK, gin.H{"batch_id": batchID, "total": len(valid)})
}

// runBatch 串行执行一个批次内的所有迁移对，一个完成后再执行下一个。
func runBatch(batchID string, ownerID uint, rows []batchRow, opts batchOptions) {
	for _, r := range rows {
		// 批次被取消时，剩余子任务直接标记为 cancelled，不再执行
		if b, err := store.GetBatchMigration(batchID); err == nil && b.Status == "cancelled" {
			break
		}

		// 构造临时连接对象（ID=0，不入 Connection 表，与连接管理隔离）
		srcConn := &store.Connection{
			DBType: r.SrcDBType, Host: r.SrcHost, Port: r.SrcPort,
			Database: r.SrcDatabase, Username: r.SrcUsername, Password: r.SrcPassword,
		}
		dstConn := &store.Connection{
			DBType: r.DstDBType, Host: r.DstHost, Port: r.DstPort,
			Database: r.DstDatabase, Username: r.DstUsername, Password: r.DstPassword,
		}

		jobID := uuid.New().String()
		ctx, cancel := context.WithCancel(context.Background())
		job := datamigrate.Registry.Register(jobID, cancel)

		dbJob := &store.DataMigrationJob{
			OwnerID:            ownerID,
			JobID:              jobID,
			BatchID:            batchID,
			SrcDBType:          r.SrcDBType,
			DstDBType:          r.DstDBType,
			MigrateMode:        "all",
			PageSize:           opts.PageSize,
			MaxParallel:        opts.MaxParallel,
			IntraTableParallel: opts.IntraTableParallel,
			LowerCaseNames:     opts.LowerCaseNames,
			CharInLength:       opts.CharInLength,
			UseNvarchar2:       opts.UseNvarchar2,
			ChangeOwner:        opts.ChangeOwner,
			DstSchema:          r.TargetSchema,
			Status:             "running",
			SrcConnName:        fmt.Sprintf("%s:%d/%s", r.SrcHost, r.SrcPort, r.SrcDatabase),
			SrcConnHost:        r.SrcHost,
			SrcConnPort:        r.SrcPort,
			SrcConnDatabase:    r.SrcDatabase,
			SrcConnUsername:    r.SrcUsername,
			DstConnName:        fmt.Sprintf("%s:%d/%s", r.DstHost, r.DstPort, r.DstDatabase),
			DstConnHost:        r.DstHost,
			DstConnPort:        r.DstPort,
			DstConnDatabase:    r.DstDatabase,
			DstConnUsername:    r.DstUsername,
		}
		if err := store.CreateDataMigrationJob(dbJob); err != nil {
			cancel()
			datamigrate.Registry.Remove(jobID)
			continue
		}

		p := migrationParams{
			MigrateMode:        "all",
			MigrateContent:     opts.MigrateContent,
			TargetSchema:       r.TargetSchema,
			StripViewSchemas:   opts.StripViewSchemas,
			PageSize:           opts.PageSize,
			MaxParallel:        opts.MaxParallel,
			IntraTableParallel: opts.IntraTableParallel,
			LowerCaseNames:     opts.LowerCaseNames,
			CharInLength:       opts.CharInLength,
			UseNvarchar2:       opts.UseNvarchar2,
			Distributed:        opts.Distributed,
			ChangeOwner:        opts.ChangeOwner,
		}

		// 后台 drain 日志，避免 LogCh 满后阻塞迁移（无 SSE 订阅者时仍能推进）
		func() {
			defer func() {
				close(job.LogCh)
				datamigrate.Registry.Remove(jobID)
				cancel()
			}()
			runDataMigration(ctx, job, dbJob, srcConn, dstConn, p)
		}()
	}

	// 批次收尾：未被取消则标记 done
	if b, err := store.GetBatchMigration(batchID); err == nil {
		now := time.Now()
		if b.Status != "cancelled" {
			b.Status = "done"
		}
		b.FinishedAt = &now
		_ = store.UpdateBatchMigration(b)
	}
}

// CancelBatch 取消整个批次：标记批次为 cancelled，并取消当前运行中的子任务。
func CancelBatch(c *gin.Context) {
	batchID := c.Param("batchID")
	b, err := store.GetBatchMigration(batchID)
	if err != nil || b == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "批次不存在"})
		return
	}
	if !middleware.IsAdmin(c) && b.OwnerID != middleware.GetCurrentUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "批次不存在"})
		return
	}
	if b.Status == "running" {
		b.Status = "cancelled"
		_ = store.UpdateBatchMigration(b)
	}
	// 取消当前正在运行的子任务（串行执行，至多一个）
	jobs, _ := store.ListJobsByBatch(batchID)
	for _, j := range jobs {
		if j.Status == "running" {
			if job := datamigrate.Registry.Get(j.JobID); job != nil {
				job.Cancel()
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"message": "已发送取消信号"})
}

// ListBatches 返回当前用户（admin 为全部）的批量迁移批次列表。
func ListBatches(c *gin.Context) {
	list, err := store.ListBatchMigrations(middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

// ListBatchJobs 返回指定批次下的全部子任务（含连接快照）。
func ListBatchJobs(c *gin.Context) {
	batchID := c.Param("batchID")
	b, err := store.GetBatchMigration(batchID)
	if err != nil || b == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "批次不存在"})
		return
	}
	if !middleware.IsAdmin(c) && b.OwnerID != middleware.GetCurrentUserID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": "批次不存在"})
		return
	}
	jobs, err := store.ListJobsByBatch(batchID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, jobs)
}

// DownloadBatchTemplate 生成并返回一个空的批量迁移 Excel 模板（含表头与一行示例）。
func DownloadBatchTemplate(c *gin.Context) {
	f := excelize.NewFile()
	defer f.Close()

	const sheet = "db_config"
	f.SetSheetName(f.GetSheetName(0), sheet)

	for i, h := range batchHeaders {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellValue(sheet, fmt.Sprintf("%s1", col), h)
	}
	// 一行示例值，提示填写格式
	example := []any{
		"mysql", "192.168.1.10", 3306, "src_db", "root", "password",
		"gaussdb", "192.168.1.20", 5432, "dst_db", "gaussuser", "password", "public",
	}
	for i, v := range example {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellValue(sheet, fmt.Sprintf("%s2", col), v)
	}

	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Header("Content-Disposition", "attachment; filename=batch_migration_template.xlsx")
	if err := f.Write(c.Writer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成模板失败"})
	}
}
