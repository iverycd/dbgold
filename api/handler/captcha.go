package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/mojocn/base64Captcha"
)

// captchaStore 验证码答案的内存存储，自带过期淘汰。
// 单实例部署够用；多实例需替换为共享存储（如 Redis）。
var captchaStore = base64Captcha.DefaultMemStore

// newCaptcha 构造一个数字+字符混合的图形验证码生成器。
// 5 位字符，240x80 图片，含少量干扰线/点。
func newCaptcha() *base64Captcha.Captcha {
	driver := base64Captcha.NewDriverString(
		80,                                 // height
		240,                                // width
		4,                                  // 干扰点数量
		2,                                  // 干扰线模式
		5,                                  // 验证码字符数
		"234567890abcdefghjkmnpqrstuvwxyz", // 去除易混淆字符（0/o、1/l/i）
		nil, nil, nil,
	)
	return base64Captcha.NewCaptcha(driver, captchaStore)
}

// IssueCaptcha 公开端点：签发一张图形验证码。
// 返回 captcha_id（提交工单时回传）与 image（base64 PNG data-uri，前端直接作为 img src）。
func IssueCaptcha(c *gin.Context) {
	id, b64, _, err := newCaptcha().Generate()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "验证码生成失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"captcha_id": id, "image": b64})
}

// verifyCaptcha 校验验证码，校验后即清除（单次有效）。
func verifyCaptcha(id, code string) bool {
	if id == "" || code == "" {
		return false
	}
	return captchaStore.Verify(id, code, true)
}
