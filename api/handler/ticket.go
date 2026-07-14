package handler

import (
	"crypto/rand"
	"dbgold/middleware"
	"dbgold/store"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// 工单上传配置（由 main 在启动时注入，见 SetUploadConfig）。
var (
	uploadDir            = "uploads"
	maxUploadBytes int64 = 50 << 30 // 50GB
)

// SetUploadConfig 注入工单离线文件的落盘目录与单文件大小上限。
func SetUploadConfig(dir string, maxBytes int64) {
	if strings.TrimSpace(dir) != "" {
		uploadDir = dir
	}
	if maxBytes > 0 {
		maxUploadBytes = maxBytes
	}
}

// allowedUploadExts 工单源库离线文件允许的扩展名（小写）。
var allowedUploadExts = map[string]bool{".sql": true, ".dmp": true}

// submitTicketRequest 为前台公开提交的迁移工单请求体。
// 源库 / 目标库各一套连接字段，db_type 取值与连接管理一致。
// 源库连接信息均可选：申请人可改为上传离线文件（src_file_path 由上传接口回填）。
type submitTicketRequest struct {
	Applicant string `json:"applicant"`
	Remark    string `json:"remark"`

	// 图形验证码：captcha_id 由 GET /api/tickets/captcha 签发，captcha_code 为用户输入。
	CaptchaID   string `json:"captcha_id" binding:"required"`
	CaptchaCode string `json:"captcha_code" binding:"required"`

	SrcDBType   string `json:"src_db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb dameng seabox highgo kingbase"`
	SrcHost     string `json:"src_host"`
	SrcPort     int    `json:"src_port" binding:"omitempty,min=1,max=65535"`
	SrcDatabase string `json:"src_database"`
	SrcUsername string `json:"src_username"`
	SrcPassword string `json:"src_password"`

	// 源库离线文件（与源库连接信息二选一）
	SrcFileName string `json:"src_file_name"`
	SrcFilePath string `json:"src_file_path"`
	SrcFileSize int64  `json:"src_file_size"`

	DstDBType   string `json:"dst_db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb dameng seabox highgo kingbase"`
	DstHost     string `json:"dst_host"`
	DstPort     int    `json:"dst_port" binding:"omitempty,min=1,max=65535"`
	DstDatabase string `json:"dst_database"`
	DstUsername string `json:"dst_username"`
	DstPassword string `json:"dst_password"`
}

type updateTicketRequest struct {
	Status    string `json:"status" binding:"required,oneof=pending processed rejected"`
	AdminNote string `json:"admin_note"`
}

// updateTicketInfoRequest 管理员修改工单连接基础信息的请求体。
// 校验规则与 submitTicketRequest 一致；源库离线文件字段（src_file_*）不在可编辑范围。
type updateTicketInfoRequest struct {
	Applicant string `json:"applicant"`
	Remark    string `json:"remark"`

	SrcDBType   string `json:"src_db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb dameng seabox highgo kingbase"`
	SrcHost     string `json:"src_host"`
	SrcPort     int    `json:"src_port" binding:"omitempty,min=1,max=65535"`
	SrcDatabase string `json:"src_database"`
	SrcUsername string `json:"src_username"`
	SrcPassword string `json:"src_password"`

	DstDBType   string `json:"dst_db_type" binding:"required,oneof=mysql postgres oracle sqlserver gaussdb dameng seabox highgo kingbase"`
	DstHost     string `json:"dst_host"`
	DstPort     int    `json:"dst_port" binding:"omitempty,min=1,max=65535"`
	DstDatabase string `json:"dst_database"`
	DstUsername string `json:"dst_username"`
	DstPassword string `json:"dst_password"`
}

// UploadTicketFile 公开端点：上传源库离线文件（.sql / .dmp，单文件最大 maxUploadBytes）。
// 采用流式落盘（MultipartReader + io.Copy），避免把 50GB 文件缓冲进内存或临时盘。
// 仅存储文件并返回落盘路径，由申请人随后随工单一并提交。
func UploadTicketFile(c *gin.Context) {
	// 限制请求体上限，超限时 reader 返回错误。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes)

	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请使用 multipart/form-data 上传文件"})
		return
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传内容失败: " + err.Error()})
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}

		// 净化原始文件名，防目录穿越。
		origName := filepath.Base(part.FileName())
		ext := strings.ToLower(filepath.Ext(origName))
		if !allowedUploadExts[ext] {
			_ = part.Close()
			c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持 .sql 或 .dmp 文件"})
			return
		}

		if err := os.MkdirAll(uploadDir, 0o755); err != nil {
			_ = part.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建上传目录失败: " + err.Error()})
			return
		}

		// 唯一存储名：<时间戳>_<随机短串>_<净化后原名>
		storedName := fmt.Sprintf("%d_%s_%s", time.Now().UnixNano(), randHex(4), sanitizeName(origName))
		storedPath := filepath.Join(uploadDir, storedName)

		dst, err := os.Create(storedPath)
		if err != nil {
			_ = part.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建文件失败: " + err.Error()})
			return
		}

		written, copyErr := io.Copy(dst, part)
		closeErr := dst.Close()
		_ = part.Close()

		if copyErr != nil {
			_ = os.Remove(storedPath) // 清理半成品
			// MaxBytesReader 截断时也走这里
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "文件写入失败或超过大小上限: " + copyErr.Error()})
			return
		}
		if closeErr != nil {
			_ = os.Remove(storedPath)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存文件失败: " + closeErr.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"stored_path":   storedPath,
			"original_name": origName,
			"size":          written,
		})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": "未找到上传文件（字段名应为 file）"})
}

// sanitizeName 去除文件名中的路径分隔符及不安全字符，保留可读性。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, " ", "_")
	if name == "" {
		return "file"
	}
	return name
}

// randHex 返回 n 字节的随机十六进制串。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// SubmitTicket 公开端点：任何人（无需登录）均可提交迁移工单。
func SubmitTicket(c *gin.Context) {
	var body submitTicketRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 校验图形验证码（单次有效，校验后即失效）。
	if !verifyCaptcha(body.CaptchaID, body.CaptchaCode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "验证码错误或已失效，请重新输入"})
		return
	}
	// 源库连接信息与离线文件二选一：都为空则拒绝。
	hasConn := strings.TrimSpace(body.SrcHost) != "" && strings.TrimSpace(body.SrcUsername) != ""
	hasFile := strings.TrimSpace(body.SrcFilePath) != ""
	if !hasConn && !hasFile {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供源库连接信息，或上传源库离线文件"})
		return
	}
	t := &store.MigrationTicket{
		Applicant: body.Applicant,
		Remark:    body.Remark,
		SrcDBType: body.SrcDBType, SrcHost: body.SrcHost, SrcPort: body.SrcPort,
		SrcDatabase: body.SrcDatabase, SrcUsername: body.SrcUsername, SrcPassword: body.SrcPassword,
		SrcFileName: body.SrcFileName, SrcFilePath: body.SrcFilePath, SrcFileSize: body.SrcFileSize,
		DstDBType: body.DstDBType, DstHost: body.DstHost, DstPort: body.DstPort,
		DstDatabase: body.DstDatabase, DstUsername: body.DstUsername, DstPassword: body.DstPassword,
		ClientIP: c.ClientIP(),
		Status:   "pending",
	}
	if err := store.CreateTicket(t); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 不回显密码
	c.JSON(http.StatusCreated, gin.H{"id": t.ID, "message": "submitted"})
}

// ListTickets 管理员端点：列出全部工单（不含密码）。
func ListTickets(c *gin.Context) {
	list, err := store.ListTickets()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

// GetTicket 管理员端点：取工单完整详情（含密码，供执行迁移时取用）。
func GetTicket(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	t, err := store.GetTicket(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ticket not found"})
		return
	}
	c.JSON(http.StatusOK, t)
}

// UpdateTicket 管理员端点：流转工单状态并记录处理备注。
func UpdateTicket(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if _, err := store.GetTicket(uint(id)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ticket not found"})
		return
	}
	var body updateTicketRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := store.UpdateTicketStatus(uint(id), body.Status, body.AdminNote); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "updated"})
}

// UpdateTicketInfo 管理员端点：修改工单的连接基础信息（源库 / 目标库）。
// 用 map 显式列出全部可编辑列，确保清空库名 / 密码、端口归零等也能写入。
func UpdateTicketInfo(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if _, err := store.GetTicket(uint(id)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ticket not found"})
		return
	}
	var body updateTicketInfoRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	fields := map[string]any{
		"applicant":    body.Applicant,
		"remark":       body.Remark,
		"src_db_type":  body.SrcDBType,
		"src_host":     body.SrcHost,
		"src_port":     body.SrcPort,
		"src_database": body.SrcDatabase,
		"src_username": body.SrcUsername,
		"src_password": body.SrcPassword,
		"dst_db_type":  body.DstDBType,
		"dst_host":     body.DstHost,
		"dst_port":     body.DstPort,
		"dst_database": body.DstDatabase,
		"dst_username": body.DstUsername,
		"dst_password": body.DstPassword,
	}
	if err := store.UpdateTicketInfo(uint(id), fields); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "updated"})
}

// upsertTicketConn 按「owner + 连接名」复用：命中则更新全部连接字段并返回其 ID，
// 未命中则新建。连接名由调用方按工单确定性生成，避免同一工单多次发起堆积重复连接。
func upsertTicketConn(ownerID uint, name, dbType, host string, port int, database, username, password string) (uint, error) {
	fields := map[string]any{
		"db_type":  dbType,
		"host":     host,
		"port":     port,
		"database": database,
		"username": username,
		"password": password,
	}
	existing, err := store.FindConnectionByOwnerName(ownerID, name)
	if err == nil {
		if err := store.UpdateConnection(existing.ID, fields); err != nil {
			return 0, err
		}
		return existing.ID, nil
	}
	conn := &store.Connection{
		OwnerID: ownerID, Name: name, DBType: dbType,
		Host: host, Port: port, Database: database, Username: username, Password: password,
	}
	if err := store.CreateConnection(conn); err != nil {
		return 0, err
	}
	return conn.ID, nil
}

// CreateTicketConnections 管理员端点：用工单信息按名复用 / 创建源库 + 目标库两个连接，
// 返回两个 conn_id 供前端跳转到迁移页预选，免去手动录入连接。
func CreateTicketConnections(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	t, err := store.GetTicket(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ticket not found"})
		return
	}
	// 源库为离线文件时没有连接信息，无法自动建连接。
	if strings.TrimSpace(t.SrcFilePath) != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库为离线文件，无法自动创建连接，请手动处理"})
		return
	}
	// 建连接要求 host / port / username 齐全（与 connectionRequest 校验一致）。
	if strings.TrimSpace(t.SrcHost) == "" || t.SrcPort <= 0 || strings.TrimSpace(t.SrcUsername) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "源库连接信息不完整（需主机、端口、用户名），请先在详情中补全"})
		return
	}
	if strings.TrimSpace(t.DstHost) == "" || t.DstPort <= 0 || strings.TrimSpace(t.DstUsername) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标库连接信息不完整（需主机、端口、用户名），请先在详情中补全"})
		return
	}

	ownerID := middleware.GetCurrentUserID(c)
	srcID, err := upsertTicketConn(ownerID, fmt.Sprintf("工单#%d-源", t.ID),
		t.SrcDBType, t.SrcHost, t.SrcPort, t.SrcDatabase, t.SrcUsername, t.SrcPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建源库连接失败: " + err.Error()})
		return
	}
	dstID, err := upsertTicketConn(ownerID, fmt.Sprintf("工单#%d-目标", t.ID),
		t.DstDBType, t.DstHost, t.DstPort, t.DstDatabase, t.DstUsername, t.DstPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建目标库连接失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"src_conn_id": srcID, "dst_conn_id": dstID})
}

// DeleteTicket 管理员端点：删除工单。
func DeleteTicket(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := store.DeleteTicket(uint(id)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}
