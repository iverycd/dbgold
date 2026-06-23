package store

import "time"

// MigrationTicket 表示前台公开提交的迁移工单。
// 与 Connection 不同：工单为匿名公开提交，没有 OwnerID，仅 admin 可在后台查看与流转。
// 源库 / 目标库各保存一套连接字段（与连接管理一致）。
type MigrationTicket struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Applicant string `json:"applicant"` // 申请人（联系方式，可选）
	Remark    string `json:"remark"`    // 申请人填写的需求说明

	// 源库（连接信息均可选：申请人可能没有现成源库连接，改为上传离线文件）
	SrcDBType   string `gorm:"not null" json:"src_db_type"`
	SrcHost     string `json:"src_host"`
	SrcPort     int    `json:"src_port"`
	SrcDatabase string `json:"src_database"`
	SrcUsername string `json:"src_username"`
	SrcPassword string `json:"src_password,omitempty"` // 列表接口会清空

	// 源库离线文件（与源库连接信息二选一：申请人无连接时上传 .sql / .dmp）
	SrcFileName string `json:"src_file_name"` // 原始文件名（展示用）
	SrcFilePath string `json:"src_file_path"` // 落盘路径（管理员取用）
	SrcFileSize int64  `json:"src_file_size"` // 字节数

	// 目标库（主机 / 端口 / 用户名 / 密码均可选：申请人可能尚未准备好目标库）
	DstDBType   string `gorm:"not null" json:"dst_db_type"`
	DstHost     string `json:"dst_host"`
	DstPort     int    `json:"dst_port"`
	DstDatabase string `json:"dst_database"`
	DstUsername string `json:"dst_username"`
	DstPassword string `json:"dst_password,omitempty"` // 列表接口会清空

	// 提交来源
	ClientIP string `json:"client_ip"` // 前台提交者 IP（公开提交，用于溯源）

	// 流转
	Status    string    `gorm:"not null;default:pending" json:"status"` // pending / processed / rejected
	AdminNote string    `json:"admin_note"`                             // 管理员处理备注
	CreatedAt time.Time `json:"created_at"`
}

func CreateTicket(t *MigrationTicket) error {
	return DB.Create(t).Error
}

// ListTickets 返回全部工单（按创建时间倒序），并清空两个密码字段，
// 避免明文密码出现在列表响应中（配合 json omitempty 直接不序列化）。
func ListTickets() ([]MigrationTicket, error) {
	var list []MigrationTicket
	if err := DB.Order("created_at desc").Find(&list).Error; err != nil {
		return nil, err
	}
	for i := range list {
		list[i].SrcPassword = ""
		list[i].DstPassword = ""
	}
	return list, nil
}

func GetTicket(id uint) (*MigrationTicket, error) {
	var t MigrationTicket
	if err := DB.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func UpdateTicketStatus(id uint, status, note string) error {
	return DB.Model(&MigrationTicket{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "admin_note": note}).Error
}

// UpdateTicketInfo 更新工单的连接基础信息（源库 / 目标库）。
// 必须用 map 而非 struct：GORM 的 struct Updates 会忽略零值字段，
// 导致清空库名 / 密码、端口改回 0 等写不进去。调用方需显式列出全部可编辑列。
func UpdateTicketInfo(id uint, fields map[string]any) error {
	return DB.Model(&MigrationTicket{}).Where("id = ?", id).Updates(fields).Error
}

func DeleteTicket(id uint) error {
	return DB.Delete(&MigrationTicket{}, id).Error
}
