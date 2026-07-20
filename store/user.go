package store

import "golang.org/x/crypto/bcrypt"

func CreateUser(username, plainPassword, role string) (*User, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{Username: username, Password: string(hashed), Role: role}
	if err := DB.Create(u).Error; err != nil {
		return nil, err
	}
	return u, nil
}

func GetUserByUsername(username string) (*User, error) {
	var u User
	if err := DB.Where("username = ?", username).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func GetUserByID(id uint) (*User, error) {
	var u User
	if err := DB.First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func ListUsers() ([]User, error) {
	var users []User
	if err := DB.Order("id").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

func UpdateUser(id uint, updates map[string]any) error {
	return DB.Model(&User{}).Where("id = ?", id).Updates(updates).Error
}

// CountEnabledAdmins 返回当前启用状态的 admin 用户数量。
func CountEnabledAdmins() (int64, error) {
	var count int64
	err := DB.Model(&User{}).Where("role = ? AND enabled = ?", "admin", true).Count(&count).Error
	return count, err
}

func EnsureAdminExists(username, plainPassword string) error {
	var count int64
	DB.Model(&User{}).Where("role = ?", "admin").Count(&count)
	if count > 0 {
		return nil
	}
	_, err := CreateUser(username, plainPassword, "admin")
	return err
}

// BackfillOwner 把存量无归属（owner_id = 0）的连接和迁移任务归到指定 admin 用户名下。
// 用 owner_id = 0 作为「未归属」判据，admin 用户 ID 不会是 0，重复执行安全（幂等）。
func BackfillOwner(adminUsername string) error {
	admin, err := GetUserByUsername(adminUsername)
	if err != nil {
		return err
	}
	if err := DB.Model(&Connection{}).Where("owner_id = ?", 0).Update("owner_id", admin.ID).Error; err != nil {
		return err
	}
	if err := DB.Model(&DataMigrationJob{}).Where("owner_id = ?", 0).Update("owner_id", admin.ID).Error; err != nil {
		return err
	}
	return nil
}
