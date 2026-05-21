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

func EnsureAdminExists(username, plainPassword string) error {
	var count int64
	DB.Model(&User{}).Where("role = ?", "admin").Count(&count)
	if count > 0 {
		return nil
	}
	_, err := CreateUser(username, plainPassword, "admin")
	return err
}
