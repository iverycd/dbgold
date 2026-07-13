package coldefault

import "testing"

func TestStrip_Dameng(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"数字默认值双引号包裹", "''0''", "0"},
		{"字符串默认值三层引号", "'''abc'''", "'abc'"},
		{"单层引号裸值", "'0'", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Strip("dameng", c.raw)
			if got != c.want {
				t.Errorf("Strip(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestStrip_UnknownSrcType 确认未注册的 srcType 仍走 default 分支原样透传,
// 避免今后新增源库时又踩到本次达梦漏注册同样的坑而不自知。
func TestStrip_UnknownSrcType(t *testing.T) {
	got := Strip("unknown", "'0'")
	if got != "'0'" {
		t.Errorf("Strip(unknown) = %q, want unchanged %q", got, "'0'")
	}
}
