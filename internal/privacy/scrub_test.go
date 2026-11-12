package privacy

import (
	"regexp"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// tagRe 按 label 构造 "[LABEL_xxxxxx]" 假名占位符的匹配正则。
// 占位符带指纹（依赖被脱敏的真实值），断言只能用正则。
func tagRe(label string) *regexp.Regexp {
	return regexp.MustCompile(`\[` + label + `_[0-9a-f]{6}\]`)
}

// tagInWantRe 匹配 QuoteMeta 之后期望串里的 \[LABEL\] 标记
//（QuoteMeta 会把两个方括号都转义）。
var tagInWantRe = regexp.MustCompile(`\\\[([A-Z_]+)\\\]`)

// wantScrubRe 把含 [LABEL] 标记的期望串展开为整体匹配正则：标记 →
// [LABEL_xxxxxx]（6 位 hex 指纹占位符），其余部分逐字匹配；无标记的
// 行退化为精确匹配。同一断言形态覆盖"被脱敏"和"原样保留"两类用例。
func wantScrubRe(want string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(want)
	expr := tagInWantRe.ReplaceAllString(quoted, `\[${1}_[0-9a-f]{6}\]`)
	return regexp.MustCompile(`^` + expr + `$`)
}

func TestScrub(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // [LABEL] 标记 = 带指纹的假名占位符
	}{
		// 邮箱
		{"email", "联系 a@b.com 谢谢", "联系 [EMAIL] 谢谢"},
		// git@ URL 不当邮箱脱（修复点）
		{"git at url", "git clone git@github.com:user/repo.git", "git clone git@github.com:user/repo.git"},
		// ssh 命令里的 user@host 不脱（修复点）
		{"ssh command", "ssh root@server.example.com uptime", "ssh root@server.example.com uptime"},
		// 中国手机号（修复点：适配中文）
		{"phone cn", "电话 13800138000 找我", "电话 [PHONE] 找我"},
		// 手机号数字边界：嵌在更长数字串里不脱
		{"phone in longer digits", "订单号 91380013800042", "订单号 91380013800042"},
		// 美国手机号（保留）
		{"phone us", "call (123) 456-7890", "call [PHONE]"},
		// 身份证
		{"id card", "身份证 11010519491231002X", "身份证 [IDCARD]"},
		// 银行卡 Luhn 通过（Visa 测试号）
		{"bankcard valid", "卡号 4111111111111111 到账", "卡号 [CARD] 到账"},
		// 13 位但 Luhn 不通过 → 不脱
		{"bankcard luhn fail", "流水 1234567890123", "流水 1234567890123"},
		// 私网 / 回环不脱（修复点：agent 友好）
		{"private ip", "内网 192.168.1.1 和 127.0.0.1", "内网 192.168.1.1 和 127.0.0.1"},
		// 公网 IP 脱
		{"public ip", "DNS 8.8.8.8 备用", "DNS [IP] 备用"},
		// 非法段（>255）不匹配
		{"ip invalid octet", "版本 1.2.3.999 这里", "版本 1.2.3.999 这里"},
		// 5 段（版本号）不脱
		{"ip five octets", "v 1.2.3.4.5 版本", "v 1.2.3.4.5 版本"},
		// API key 各格式
		{"openai key", "key sk-proj-abcdef1234567890abcdef", "key [API_KEY]"},
		{"aws key", "AKIAIOSFODNN7EXAMPLE ok", "[API_KEY] ok"},
		{"github token", "ghp_abcdefghijklmnopqrstuvwxyz0123456789AB", "[API_KEY]"},
		// JWT
		{"jwt", "Bearer eyJhbGci.eyJzdWIi-x.SflKxwRJSignature", "Bearer [TOKEN]"},
		// 上下文口令：保留 key，脱 value
		{"context secret en", "password: Hunter2xyz leaked", "password: [SECRET] leaked"},
		{"context secret zh", "我的密码是 Hj8kQp2nR4 secret", "我的密码是 [SECRET] secret"},
		// JSON password 字段
		{"json password", `{"name":"a","password":"secret123"}`, `{"name":"a","password":"[SECRET]"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scrub(tt.in); !wantScrubRe(tt.want).MatchString(got) {
				t.Errorf("Scrub(%q)\n  got  = %q\n  want = %q（[LABEL] = 带指纹的假名占位符）", tt.in, got, tt.want)
			}
		})
	}
}

func TestScrubHighEntropyOffByDefault(t *testing.T) {
	// 默认 Options 不开高熵：随机串即使周围有密钥词也不脱
	in := "api_key 的值 xJ7kQp2nR4sT9vB1wY5z 已生成"
	if got := Scrub(in); got != in {
		t.Errorf("默认应关闭高熵兜底，got = %q", got)
	}
}

func TestScrubHighEntropyConservative(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// 有上下文 + 高熵 → 脱
		{"ctx high entropy", "api_key 的值 xJ7kQp2nR4sT9vB1wY5z 已生成", "api_key 的值 [SECRET] 已生成"},
		// 无上下文 + 高熵 → 不脱（保守版核心）
		{"no ctx high entropy", "随机串 xJ7kQp2nR4sT9vB1wY5z 结束", "随机串 xJ7kQp2nR4sT9vB1wY5z 结束"},
		// 标准 hash（32 hex）即使有上下文也不脱
		{"hex hash", "token e4b9a3f7c2d819f0605a2e7c4b8d3a9f 值", "token e4b9a3f7c2d819f0605a2e7c4b8d3a9f 值"},
		// UUID 不脱
		{"uuid", "id 12345678-1234-1234-1234-1234567890ab end", "id 12345678-1234-1234-1234-1234567890ab end"},
		// 模板变量不脱
		{"template var", "key ${{SECRET_TOKEN}} here", "key ${{SECRET_TOKEN}} here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScrubWith(tt.in, Options{Entropy: true}); !wantScrubRe(tt.want).MatchString(got) {
				t.Errorf("ScrubWith(Entropy=true)(%q)\n  got  = %q\n  want = %q（[LABEL] = 带指纹的假名占位符）", tt.in, got, tt.want)
			}
		})
	}
}

func TestScrubMessagesWithOpts(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "邮箱 a@b.com，密码是 Hunter2xyz"},
		{Role: "assistant", Content: "ok 13800138000"},
	}
	out := ScrubMessages(msgs, Options{Entropy: true})
	if !wantScrubRe("邮箱 [EMAIL]，密码是 [SECRET]").MatchString(out[0].Content) {
		t.Errorf("msg0 = %q，want 邮箱 [EMAIL]，密码是 [SECRET]（带指纹）", out[0].Content)
	}
	if !wantScrubRe("ok [PHONE]").MatchString(out[1].Content) {
		t.Errorf("msg1 = %q，want ok [PHONE]（带指纹）", out[1].Content)
	}
}

// TestScrubFingerprintStable 锁定假名占位符的两个核心性质：
// 同值同标签（跨轮 scrub 是对原始历史的重放，标签必须字节级稳定，
// 否则前缀缓存全灭、模型会误判"换号"）；异值异标签（模型靠标签
// 区分不同的号码 / 不同的 key）。
func TestScrubFingerprintStable(t *testing.T) {
	tagOf := func(in string) string {
		m := tagRe("PHONE").FindStringSubmatch(Scrub(in))
		if m == nil {
			t.Fatalf("Scrub(%q) 未产出 PHONE 占位符", in)
		}
		return m[0]
	}
	a := tagOf("电话 13800138000 找我")
	if b := tagOf("稍后电话 13800138000"); a != b {
		t.Errorf("同值应同标签：%q vs %q", a, b)
	}
	if c := tagOf("电话 13900139000 找我"); c == a {
		t.Errorf("异值应异标签：同得 %q", c)
	}
}

func TestContainsPII(t *testing.T) {
	if !ContainsPII("联系 a@b.com") {
		t.Error("应检测到 PII")
	}
	if ContainsPII("普通文本没有敏感信息") {
		t.Error("不应误报 PII")
	}
}
