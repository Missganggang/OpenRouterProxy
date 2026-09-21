package util

import (
	"io"
	"net/http"
	"testing"
)

// newRequest 构造一个最小的测试请求，避免测试文件引入 httptest 之外的依赖。
func newRequest(method, target string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, target, body)
}

// TestMatchHostSuffix 覆盖规格书 4.3 定义的后缀匹配语义。
//
// 这是入口组黑白名单的核心逻辑，语义错一个字符就会导致误放行或误拦截。
func TestMatchHostSuffix(t *testing.T) {
	cases := []struct {
		host string
		rule string
		want bool
	}{
		// 规格书示例：.qq.com 匹配 a.qq.com
		{"a.qq.com", ".qq.com", true},
		{"qq.com", ".qq.com", false},    // 带点号规则不匹配自身
		{"b.a.qq.com", ".qq.com", true}, // 多级子域
		{"xqq.com", ".qq.com", false},   // 必须以点号边界结尾，不能是子串匹配

		// 不带点号：匹配自身与子域
		{"qq.com", "qq.com", true},
		{"a.qq.com", "qq.com", true},
		{"xqq.com", "qq.com", false},

		// 通配
		{"anything.example", "*", true},

		// 大小写不敏感
		{"A.QQ.COM", ".qq.com", true},

		// 空规则不匹配任何东西
		{"a.qq.com", "", false},
	}
	for _, c := range cases {
		if got := MatchHostSuffix(c.host, c.rule); got != c.want {
			t.Errorf("MatchHostSuffix(%q, %q) = %v, 期望 %v", c.host, c.rule, got, c.want)
		}
	}
}

// TestCheckHostPolicy 验证黑名单优先级高于白名单。
func TestCheckHostPolicy(t *testing.T) {
	allowed := []string{".example.com"}
	blocked := []string{".bad.example.com"}

	// 同时命中白名单与黑名单时应被拒绝（黑名单优先）。
	if ok, _ := CheckHostPolicy("x.bad.example.com", allowed, blocked); ok {
		t.Error("同时命中黑白名单时应拒绝（黑名单优先）")
	}
	// 仅命中白名单应放行。
	if ok, reason := CheckHostPolicy("a.example.com", allowed, blocked); !ok {
		t.Errorf("命中白名单应放行，却被拒绝: %s", reason)
	}
	// 不在白名单内应拒绝。
	if ok, _ := CheckHostPolicy("evil.com", allowed, blocked); ok {
		t.Error("不在白名单内应拒绝")
	}
	// 白名单为空表示不限制。
	if ok, _ := CheckHostPolicy("anything.com", nil, nil); !ok {
		t.Error("白名单为空时不应限制")
	}
}

// TestParsePortRange 覆盖单端口、端口段与非法输入。
func TestParsePortRange(t *testing.T) {
	if s, e, err := ParsePortRange("8443"); err != nil || s != 8443 || e != 0 {
		t.Errorf("单端口解析失败: %d %d %v", s, e, err)
	}
	if s, e, err := ParsePortRange("8443-8450"); err != nil || s != 8443 || e != 8450 {
		t.Errorf("端口段解析失败: %d %d %v", s, e, err)
	}
	if _, _, err := ParsePortRange("8450-8443"); err == nil {
		t.Error("结束值小于起始值应报错")
	}
	if _, _, err := ParsePortRange("0"); err == nil {
		t.Error("端口 0 应报错")
	}
	if _, _, err := ParsePortRange("70000"); err == nil {
		t.Error("端口 70000 应报错")
	}
	if _, _, err := ParsePortRange("abc"); err == nil {
		t.Error("非数字应报错")
	}
}

// TestParseSpeed 验证速率单位的换算，特别注意 Gbps 是比特单位。
func TestParseSpeed(t *testing.T) {
	cases := []struct {
		value float64
		unit  string
		want  int64
	}{
		{1, "KB/s", 1024},
		{1, "MB/s", 1024 * 1024},
		{1, "GB/s", 1024 * 1024 * 1024},
		{1, "Gbps", 125000000}, // 1 Gbps = 1000^3/8 byte/s
		{1, "Mbps", 131072},    // 1 Mbps = 1024^2/8
		{0, "", 0},
		{2.5, "MB/s", 2621440},
	}
	for _, c := range cases {
		got, err := ParseSpeed(c.value, c.unit)
		if err != nil {
			t.Errorf("ParseSpeed(%v, %q) 报错: %v", c.value, c.unit, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSpeed(%v, %q) = %d, 期望 %d", c.value, c.unit, got, c.want)
		}
	}
	if _, err := ParseSpeed(1, "光速"); err == nil {
		t.Error("非法单位应报错")
	}
}

// TestEqualFoldName 验证跨方言的名称归一化比较（规格书 4.1）。
func TestEqualFoldName(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Admin", "admin", true},
		{"HK-01", "hk-01", true},
		{"admin", "admin2", false},
		{" admin ", "ADMIN", true}, // 两侧空白应被忽略
	}
	for _, c := range cases {
		if got := EqualFoldName(c.a, c.b); got != c.want {
			t.Errorf("EqualFoldName(%q, %q) = %v, 期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// TestDedupeNamesFold 验证迁移预检报告使用的冲突检测。
func TestDedupeNamesFold(t *testing.T) {
	groups := DedupeNamesFold([]string{"Admin", "admin", "user1", "HK-01", "hk-01", "unique"})
	if len(groups) != 2 {
		t.Fatalf("应检出 2 组冲突，实际 %d 组: %v", len(groups), groups)
	}
	if len(groups["admin"]) != 2 {
		t.Errorf("admin 组应有 2 个成员: %v", groups["admin"])
	}
	if _, ok := groups["unique"]; ok {
		t.Error("非冲突项不应出现在结果中")
	}
}

// TestValidMultiplier 验证倍率范围与小数位限制（规格书 6.4）。
func TestValidMultiplier(t *testing.T) {
	for _, v := range []float64{0, 1, 1.5, 2.25, 100} {
		if err := ValidMultiplier(v); err != nil {
			t.Errorf("倍率 %v 应当合法，却报错: %v", v, err)
		}
	}
	for _, v := range []float64{-0.1, 100.1, 1.234} {
		if err := ValidMultiplier(v); err == nil {
			t.Errorf("倍率 %v 应当非法", v)
		}
	}
}

// TestParseIntList 验证 Nyanpass「限制出口」语法的解析（规格书 6.3）。
func TestParseIntList(t *testing.T) {
	ids, ban := ParseIntList("1,2,3")
	if ban || len(ids) != 3 || ids[2] != 3 {
		t.Errorf("普通列表解析失败: %v ban=%v", ids, ban)
	}

	ids, ban = ParseIntList("1,2,3,禁止单端")
	if !ban || len(ids) != 3 {
		t.Errorf("含「禁止单端」的列表解析失败: %v ban=%v", ids, ban)
	}

	// 规格书示例：用一个不存在的 ID 等价于「只允许单端」。
	ids, ban = ParseIntList("1145141919")
	if ban || len(ids) != 1 || ids[0] != 1145141919 {
		t.Errorf("大 ID 解析失败: %v", ids)
	}

	if ids, ban := ParseIntList(""); len(ids) != 0 || ban {
		t.Errorf("空串应返回空列表: %v", ids)
	}
}

// TestHashIPStable 验证加盐哈希的稳定性与不可逆性。
func TestHashIPStable(t *testing.T) {
	a := HashIP("1.2.3.4", "salt")
	b := HashIP("1.2.3.4", "salt")
	c := HashIP("1.2.3.4", "other")
	if a != b {
		t.Error("同一 IP 与盐应得到相同哈希")
	}
	if a == c {
		t.Error("不同盐应得到不同哈希")
	}
	if len(a) != 64 {
		t.Errorf("SHA256 十六进制应为 64 字符，得到 %d", len(a))
	}
}

// TestClientIPXForwardedFor 验证代理场景下的客户端 IP 提取。
func TestClientIPXForwardedFor(t *testing.T) {
	req, _ := newRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	if got := ClientIP(req); got != "203.0.113.5" {
		t.Errorf("应取 XFF 最左值，得到 %q", got)
	}

	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "198.51.100.7")
	if got := ClientIP(req); got != "198.51.100.7" {
		t.Errorf("应取 X-Real-IP，得到 %q", got)
	}
}

// TestIsBcryptHash 验证迁移时的哈希格式识别。
func TestIsBcryptHash(t *testing.T) {
	if !IsBcryptHash("$2a$10$abcdefghijklmnopqrstuv") {
		t.Error("$2a$ 前缀应识别为 bcrypt")
	}
	if IsBcryptHash("5f4dcc3b5aa765d61d8327deb882cf99") {
		t.Error("md5 不应识别为 bcrypt")
	}
}

// TestTruncateUTF8Safe 验证截断不会切断多字节字符。
func TestTruncateUTF8Safe(t *testing.T) {
	// 「中文」每个字符 3 字节，截断到 4 字节时应只保留 1 个完整字符。
	got := Truncate("中文abc", 4)
	if got != "中" {
		t.Errorf("截断结果 = %q，期望 %q", got, "中")
	}
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("未超长时不应改动，得到 %q", got)
	}
}
