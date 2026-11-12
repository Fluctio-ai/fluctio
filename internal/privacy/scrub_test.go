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

func TestScrub(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string // 精确匹配（label 为空时）
		prefix string // label 非空时：期望整体 = prefix + 占位符 + suffix
		suffix string
		label  string
	}{
		// 邮箱
		{name: "email", in: "联系 a@b.com 谢谢", prefix: "联系 ", suffix: " 谢谢", label: "EMAIL"},
		// git@ URL 不当邮箱脱（修复点）
		{name: "git at url", in: "git clone git@github.com:user/repo.git", want: "git clone git@github.com:user/repo.git"},
		// ssh 命令里的 user@host 不脱（修复点）
		{name: "ssh command", in: "ssh root@server.example.com uptime", want: "ssh root@server.example.com uptime"},
		// 中国手机号（修复点：适配中文）
		{name: "phone cn", in: "电话 13800138000 找我", prefix: "电话 ", suffix: " 找我", label: "PHONE"},
		// 手机号数字边界：嵌在更长数字串里不脱
		{name: "phone in longer digits", in: "订单号 91380013800042", want: "订单号 91380013800042"},
		// 美国手机号（保留）
		{name: "phone us", in: "call (123) 456-7890", prefix: "call ", label: "PHONE"},
		// 身份证
		{name: "id card", in: "身份证 11010519491231002X", prefix: "身份证 ", label: "IDCARD"},
		// 银行卡 Luhn 通过（Visa 测试号）
		{name: "bankcard valid", in: "卡号 4111111111111111 到账", prefix: "卡号 ", suffix: " 到账", label: "CARD"},
		// 13 位但 Luhn 不通过 → 不脱
		{name: "bankcard luhn fail", in: "流水 1234567890123", want: "流水 1234567890123"},
		// 私网 / 回环不脱（修复点：agent 友好）
		{name: "private ip", in: "内网 192.168.1.1 和 127.0.0.1", want: "内网 192.168.1.1 和 127.0.0.1"},
		// 公网 IP 脱
		{name: "public ip", in: "DNS 8.8.8.8 备用", prefix: "DNS ", suffix: " 备用", label: "IP"},
		// 非法段（>255）不匹配
		{name: "ip invalid octet", in: "版本 1.2.3.999 这里", want: "版本 1.2.3.999 这里"},
		// 5 段（版本号）不脱
		{name: "ip five octets", in: "v 1.2.3.4.5 版本", want: "v 1.2.3.4.5 版本"},
		// API key 各格式
		{name: "openai key", in: "key sk-proj-abcdef1234567890abcdef", prefix: "key ", label: "API_KEY"},
		{name: "aws key", in: "AKIAIOSFODNN7EXAMPLE ok", suffix: " ok", label: "API_KEY"},
		{name: "github token", in: "ghp_abcdefghijklmnopqrstuvwxyz0123456789AB", label: "API_KEY"},
		// JWT
		{name: "jwt", in: "Bearer eyJhbGci.eyJzdWIi-x.SflKxwRJSignature", prefix: "Bearer ", label: "TOKEN"},
		// 上下文口令：保留 key，脱 value
		{name: "context secret en", in: "password: Hunter2xyz leaked", prefix: "password: ", suffix: " leaked", label: "SECRET"},
		{name: "context secret zh", in: "我的密码是 Hj8kQp2nR4 secret", prefix: "我的密码是 ", suffix: " secret", label: "SECRET"},
		// JSON password 字段
		{name: "json password", in: `{"name":"a","password":"secret123"}`, prefix: `{"name":"a","password":"`, suffix: `"}`, label: "SECRET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scrub(tt.in)
			if tt.label == "" {
				if got != tt.want {
					t.Errorf("Scrub(%q)\n  got  = %q\n  want = %q", tt.in, got, tt.want)
				}
				return
			}
			re := regexp.MustCompile(`^` + regexp.QuoteMeta(tt.prefix) + tagRe(tt.label).String() + regexp.QuoteMeta(tt.suffix) + `$`)
			if !re.MatchString(got) {
				t.Errorf("Scrub(%q)\n  got  = %q\n  want = %s 形式的假名占位符", tt.in, got, re)
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
		name   string
		in     string
		want   string
		prefix string
		suffix string
		label  string
	}{
		// 有上下文 + 高熵 → 脱
		{name: "ctx high entropy", in: "api_key 的值 xJ7kQp2nR4sT9vB1wY5z 已生成", prefix: "api_key 的值 ", suffix: " 已生成", label: "SECRET"},
		// 无上下文 + 高熵 → 不脱（保守版核心）
		{name: "no ctx high entropy", in: "随机串 xJ7kQp2nR4sT9vB1wY5z 结束", want: "随机串 xJ7kQp2nR4sT9vB1wY5z 结束"},
		// 标准 hash（32 hex）即使有上下文也不脱
		{name: "hex hash", in: "token e4b9a3f7c2d819f0605a2e7c4b8d3a9f 值", want: "token e4b9a3f7c2d819f0605a2e7c4b8d3a9f 值"},
		// UUID 不脱
		{name: "uuid", in: "id 12345678-1234-1234-1234-1234567890ab end", want: "id 12345678-1234-1234-1234-1234567890ab end"},
		// 模板变量不脱
		{name: "template var", in: "key ${{SECRET_TOKEN}} here", want: "key ${{SECRET_TOKEN}} here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubWith(tt.in, Options{Entropy: true})
			if tt.label == "" {
				if got != tt.want {
					t.Errorf("ScrubWith(Entropy=true)(%q)\n  got  = %q\n  want = %q", tt.in, got, tt.want)
				}
				return
			}
			re := regexp.MustCompile(`^` + regexp.QuoteMeta(tt.prefix) + tagRe(tt.label).String() + regexp.QuoteMeta(tt.suffix) + `$`)
			if !re.MatchString(got) {
				t.Errorf("ScrubWith(Entropy=true)(%q)\n  got  = %q\n  want = %s 形式的假名占位符", tt.in, got, re)
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
	re0 := regexp.MustCompile(`^邮箱 ` + tagRe("EMAIL").String() + `，密码是 ` + tagRe("SECRET").String() + `$`)
	if !re0.MatchString(out[0].Content) {
		t.Errorf("msg0 = %q，want %s 形式", out[0].Content, re0)
	}
	re1 := regexp.MustCompile(`^ok ` + tagRe("PHONE").String() + `$`)
	if !re1.MatchString(out[1].Content) {
		t.Errorf("msg1 = %q，want %s 形式", out[1].Content, re1)
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
