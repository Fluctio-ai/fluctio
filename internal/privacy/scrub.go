package privacy

import (
	"math"
	"regexp"
	"strings"

	"github.com/fluctio-ai/fluctio/internal/provider"
)

// Options 控制脱敏行为。
type Options struct {
	// Entropy 启用高熵兜底（默认关）。
	// 仅当候选串周围出现密钥语义词（password / token / api_key / 密码 等）
	// 时才查香农熵，避免误伤 base64 数据、hash、UUID 等合法高熵串。
	Entropy bool
}

// 结构化 PII 正则。Go 的 regexp 是 RE2，不支持前后向断言，
// 数字边界用 digitBounded / ipBounded 在匹配后手工校验。
var (
	emailRe    = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	phoneCNRe  = regexp.MustCompile(`(?:\+?86[-\s]?)?1[3-9][0-9]{9}`)                       // 中国手机号
	phoneUSRe  = regexp.MustCompile(`(?:\+\d{1,3}[-.\s]?)?\(?\d{3}\)?[-.\s]\d{3}[-.\s]\d{4}`) // 美国手机号（分隔符必选，避免误伤裸数字串）
	idCardRe   = regexp.MustCompile(`[1-9][0-9]{16}[0-9Xx]`)                                 // 中国身份证 18 位
	bankCardRe = regexp.MustCompile(`[0-9]{13,19}`)                                          // 银行卡（+ Luhn 校验）
	ssnRe      = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	ipv4Re     = regexp.MustCompile(`(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])(?:\.(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])){3}`)
)

// 密钥 / 凭证正则。
var (
	apiKeyRe     = regexp.MustCompile(`\b(?:sk-(?:proj-)?[A-Za-z0-9_\-]{20,}|AIza[A-Za-z0-9_\-]{30,}|gh[pousr]_[A-Za-z0-9]{36,}|AKIA[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9\-]{10,}|ya29\.[A-Za-z0-9_\-]{20,}|sk_(?:live|test)_[A-Za-z0-9]{20,})\b`)
	jwtRe        = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`)
	privateKeyRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// 上下文口令：藏在句子里的 password: xxx / 密码是 xxx。
// 替换时只脱 value（第 2 组），保留 key。
var contextSecretRe = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_\s-]?key|access[_\s-]?key|bearer|authorization|密码|口令|密钥|凭证|令牌)\s*(?:是|为|[:：=])\s*['"]?([^\s'",。；;]{4,})`)

// JSON password 字段。
var jsonPasswordRe = regexp.MustCompile(`(?i)("password"\s*:\s*)"[^"]*"`)

// 高熵兜底（仅 Options.Entropy=true 时启用）。
var (
	entropyTokenRe  = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{20,}`)
	secretContextRe = regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|token|api[_\s-]?key|access[_\s-]?key|bearer|authorization|credential|jwt|密码|口令|密钥|凭证|令牌|鉴权)`)
	templateVarRe   = regexp.MustCompile(`^(?:\{\{[^{}]+\}\}|\$\{[^{}]+\}|%\{[^{}]+\}|<[^<>]+>)$`)
	uuidRe          = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexOnlyRe       = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

const (
	entropyContextLookback = 30
	entropyThreshold       = 4.0 // 保守版阈值：≈ 纯 hex（16 种字符）的均匀度
)

// 常见占位符词。上下文口令的 value 含这些词时跳过 —— 占位符不是真值。
var commonPlaceholders = []string{
	"REPLACE_ME", "REPLACE_THIS", "REPLACE_WITH",
	"YOUR_KEY", "YOUR_TOKEN", "YOUR_SECRET", "YOUR_API_KEY", "YOUR_PASSWORD",
	"INSERT_HERE", "PLACEHOLDER", "EXAMPLE_KEY", "EXAMPLE_TOKEN",
	"TODO", "FIXME", "XXXX", "CHANGEME",
}

// ssh 命令前缀。user@host 在这些命令里是 SSH 目标，不是邮箱。
var sshCommands = []string{"ssh ", "scp ", "rsync ", "sftp ", "ssh-copy-id ", "ssh-keygen "}

// Scrub 替换文本中的 PII / 密钥为占位符（默认 Options）。
func Scrub(text string) string {
	return ScrubWith(text, Options{})
}

// ScrubWith 同 Scrub，但接受 Options 控制高熵兜底等行为。
func ScrubWith(text string, opts Options) string {
	// 顺序敏感：先替换长的 / 具体的，避免被宽规则吃掉
	text = privateKeyRe.ReplaceAllString(text, "[PRIVATE_KEY]")
	text = jwtRe.ReplaceAllString(text, "[TOKEN]")
	text = apiKeyRe.ReplaceAllString(text, "[API_KEY]")
	text = replaceIDCards(text)   // 身份证 18 位（先于银行卡，避免被 Luhn 误标）
	text = replaceBankCards(text) // 银行卡 13-19 位 + Luhn
	text = ssnRe.ReplaceAllString(text, "[SSN]")
	text = replaceEmails(text) // 邮箱（排除 git@ URL / ssh 命令）
	text = replaceIPv4(text)  // IP（边界 + 私网白名单）
	text = replacePhones(text)
	text = jsonPasswordRe.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = replaceContextSecrets(text) // 上下文口令（保留 key，脱 value）
	if opts.Entropy {
		text = replaceHighEntropy(text)
	}
	return text
}

// ScrubMessages 批量脱敏消息内容（含多模态 text 部分）。
func ScrubMessages(messages []provider.Message, opts Options) []provider.Message {
	out := make([]provider.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		out[i].Content = ScrubWith(m.Content, opts)
		if len(m.ContentParts) > 0 {
			parts := make([]provider.ContentPart, len(m.ContentParts))
			copy(parts, m.ContentParts)
			for j, p := range parts {
				if p.Type == "text" {
					parts[j].Text = ScrubWith(p.Text, opts)
				}
			}
			out[i].ContentParts = parts
		}
	}
	return out
}

// ContainsPII 判断文本是否含可检测的 PII。
func ContainsPII(text string) bool {
	return Scrub(text) != text
}

// replaceWith 找出 re 的所有匹配，对每个匹配调 keep(text, start, end)
// 决定是否替换为 placeholder（keep 返回 true = 替换）。单遍重建，O(n)。
func replaceWith(text string, re *regexp.Regexp, placeholder string, keep func(text string, start, end int) bool) string {
	var b strings.Builder
	prev := 0
	matched := false
	for _, loc := range re.FindAllStringIndex(text, -1) {
		s, e := loc[0], loc[1]
		if keep != nil && !keep(text, s, e) {
			continue
		}
		if !matched {
			matched = true
		}
		b.WriteString(text[prev:s])
		b.WriteString(placeholder)
		prev = e
	}
	if !matched {
		return text
	}
	b.WriteString(text[prev:])
	return b.String()
}

func replaceEmails(text string) string {
	return replaceWith(text, emailRe, "[EMAIL]", func(t string, s, e int) bool {
		// git@ URL：邮箱后紧跟 ':' + 非空白 → git@host:path，不当邮箱
		if e < len(t) && t[e] == ':' && e+1 < len(t) && t[e+1] != ' ' && t[e+1] != '\t' {
			return false
		}
		// ssh 命令上下文：ssh user@host 这种调用，host 不是邮箱
		if isInSSHCommandContext(t, s) {
			return false
		}
		return true
	})
}

func replaceIDCards(text string) string {
	return replaceWith(text, idCardRe, "[IDCARD]", func(t string, s, e int) bool {
		return digitBounded(t, s, e)
	})
}

func replaceBankCards(text string) string {
	return replaceWith(text, bankCardRe, "[CARD]", func(t string, s, e int) bool {
		return digitBounded(t, s, e) && luhnValid(t[s:e])
	})
}

func replaceIPv4(text string) string {
	return replaceWith(text, ipv4Re, "[IP]", func(t string, s, e int) bool {
		if !ipBounded(t, s, e) {
			return false
		}
		// 私网 / 回环 / 链路本地段不脱（agent 常需操作内网 IP）
		return !isPrivateIPv4(t[s:e])
	})
}

func replacePhones(text string) string {
	// 中国手机号：裸 11 位，需数字边界防误伤
	text = replaceWith(text, phoneCNRe, "[PHONE]", func(t string, s, e int) bool {
		return digitBounded(t, s, e)
	})
	// 美国手机号：自带分隔符 / 括号结构，保留现有行为
	text = phoneUSRe.ReplaceAllString(text, "[PHONE]")
	return text
}

// replaceContextSecrets 只脱 value，保留 key + 分隔符。
func replaceContextSecrets(text string) string {
	return contextSecretRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := contextSecretRe.FindStringSubmatchIndex(m)
		if sub == nil || len(sub) < 6 || sub[4] < 0 {
			return m
		}
		value := m[sub[4]:sub[5]]
		if isLikelyPlaceholder(value) {
			return m
		}
		// 低熵短串（占位符 / 短词）跳过
		if len(value) <= 16 && shannonEntropy(value) < 3.0 {
			return m
		}
		return m[:sub[4]] + "[REDACTED]"
	})
}

// replaceHighEntropy 高熵兜底（保守版）：仅密钥上下文命中时才查熵。
func replaceHighEntropy(text string) string {
	return replaceWith(text, entropyTokenRe, "[REDACTED]", func(t string, s, e int) bool {
		cand := t[s:e]
		// 排除：模板变量 / 标准 hash / UUID
		if isTemplateVar(cand) || isHexHash(cand) || isUUID(cand) {
			return false
		}
		// 保守版：必须有密钥语义上下文
		if !hasSecretContext(t, s, e) {
			return false
		}
		return shannonEntropy(cand) >= entropyThreshold
	})
}

// ---- 校验 / 工具函数 ----

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// digitBounded 校验匹配两侧不是数字（替代 RE2 没有的前后向断言）。
func digitBounded(text string, start, end int) bool {
	if start > 0 && isDigit(text[start-1]) {
		return false
	}
	if end < len(text) && isDigit(text[end]) {
		return false
	}
	return true
}

// ipBounded 校验匹配两侧不是数字或点。
func ipBounded(text string, start, end int) bool {
	if start > 0 && (isDigit(text[start-1]) || text[start-1] == '.') {
		return false
	}
	if end < len(text) && (isDigit(text[end]) || text[end] == '.') {
		return false
	}
	return true
}

// luhnValid 做 Luhn 校验，过滤「长得像卡号的普通数字串」。
func luhnValid(num string) bool {
	sum := 0
	double := false
	for i := len(num) - 1; i >= 0; i-- {
		d := int(num[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// isPrivateIPv4 判断是否私网 / 回环 / 链路本地段（不脱）。
func isPrivateIPv4(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return false
	}
	oct := func(s string) int {
		n := 0
		for i := 0; i < len(s); i++ {
			n = n*10 + int(s[i]-'0')
		}
		return n
	}
	a, b := oct(parts[0]), oct(parts[1])
	switch {
	case a == 10: // 10.0.0.0/8
		return true
	case a == 172 && b >= 16 && b <= 31: // 172.16.0.0/12
		return true
	case a == 192 && b == 168: // 192.168.0.0/16
		return true
	case a == 127: // 127.0.0.0/8 回环
		return true
	case a == 169 && b == 254: // 169.254.0.0/16 链路本地
		return true
	}
	return false
}

// isInSSHCommandContext 检查 email 命中所在行是否含 ssh/scp/rsync 命令前缀。
func isInSSHCommandContext(text string, emailStart int) bool {
	lineStart := strings.LastIndexByte(text[:emailStart], '\n') + 1
	line := text[lineStart:emailStart]
	for _, cmd := range sshCommands {
		if strings.Contains(line, cmd) {
			return true
		}
	}
	return false
}

func isLikelyPlaceholder(s string) bool {
	upper := strings.ToUpper(s)
	for _, p := range commonPlaceholders {
		if strings.Contains(upper, p) {
			return true
		}
	}
	return false
}

// hasSecretContext 检查 [start-lookback, end) 区间是否出现密钥语义关键词。
func hasSecretContext(text string, start, end int) bool {
	lo := start - entropyContextLookback
	if lo < 0 {
		lo = 0
	}
	return secretContextRe.MatchString(text[lo:end])
}

func isTemplateVar(s string) bool { return templateVarRe.MatchString(s) }
func isUUID(s string) bool        { return uuidRe.MatchString(s) }

func isHexHash(s string) bool {
	n := len(s)
	return (n == 32 || n == 40 || n == 64) && hexOnlyRe.MatchString(s)
}

// shannonEntropy 按字节计算香农熵（bits/byte）。
func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	ent := 0.0
	for _, c := range freq {
		if c > 0 {
			p := c / n
			ent -= p * math.Log2(p)
		}
	}
	return ent
}
