package privacy

import (
	"regexp"
	"strings"
)

// 用户自定义敏感词（RedactWords，借鉴 yetone/magpie, MIT）与
// 用户名/主机名规则。含 CJK 的词按子串匹配（中文无词边界）；纯
// ASCII 的词按整词边界 + 大小写折叠匹配，避免 cat 咬住 concatenate。

// replaceWords 把 text 中命中用户敏感词的部分换成 [WORD_xxxxxx]。
// 词表按长度降序尝试，长词优先（"夜莺项目" 先于 "夜莺"）。
func replaceWords(text string, words []string, key []byte) string {
	if len(words) == 0 {
		return text
	}
	// 按首字节分桶 + 桶内长度降序：每个位置只尝试首字节命中的词。
	// ASCII 词额外注册大小写两个首字节桶（大小写折叠匹配）。
	byFirst := map[byte][]string{}
	for _, w := range words {
		if w == "" {
			continue
		}
		byFirst[w[0]] = append(byFirst[w[0]], w)
		if isASCIIWord(w) {
			lo, up := w[0]|0x20, w[0]&^0x20
			if lo != w[0] {
				byFirst[lo] = append(byFirst[lo], w)
			}
			if up != w[0] && up != lo {
				byFirst[up] = append(byFirst[up], w)
			}
		}
	}
	for _, ws := range byFirst {
		sortDesc(ws)
	}
	var b strings.Builder
	i := 0
	for i < len(text) {
		ws := byFirst[text[i]]
		matched := false
		for _, w := range ws {
			if len(w) > len(text)-i {
				continue
			}
			if isASCIIWord(w) {
				end := i + len(w)
				// ASCII 词大小写折叠 + 整词边界
				if !strings.EqualFold(text[i:end], w) || !asciiWordBounded(text, i, end) {
					continue
				}
				b.WriteString(tagged("WORD", text[i:end], key))
				i = end
			} else {
				if !strings.HasPrefix(text[i:], w) {
					continue
				}
				b.WriteString(tagged("WORD", w, key))
				i += len(w)
			}
			matched = true
			break
		}
		if !matched {
			b.WriteByte(text[i])
			i++
		}
	}
	return b.String()
}

// isASCIIWord says w 是纯 ASCII 词（按整词边界匹配）；含 CJK 的词
// 走子串匹配。
func isASCIIWord(w string) bool {
	for i := 0; i < len(w); i++ {
		if w[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiWordBounded says [start,end) 两侧都不是字母 / 数字 / _。
func asciiWordBounded(text string, start, end int) bool {
	if start > 0 && isWordByte(text[start-1]) {
		return false
	}
	if end < len(text) && isWordByte(text[end]) {
		return false
	}
	return true
}

func isWordByte(b byte) bool {
	return b < 0x80 && (b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z')
}

// sortDesc 按字节序长度降序（简单插入，词表很小）。
func sortDesc(ws []string) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && len(ws[j]) > len(ws[j-1]); j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

// homeUserRe 匹配系统家目录路径的用户名段：/home/<u>、/Users/<u>
// （macOS）、C:\Users\<u>（Windows）。只脱用户名段，路径其余部分
// 保留可读性。
var homeUserRe = regexp.MustCompile(`(?i)([a-z]:\\users\\|/home/|/users/)([^/\\\s]{1,32})`)

// replaceHomeUsers 把家目录路径中的用户名换成 [USERNAME_xxxxxx]。
func replaceHomeUsers(text string, key []byte) string {
	idx := homeUserRe.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		return text
	}
	var b strings.Builder
	prev := 0
	replaced := false
	for _, m := range idx {
		seg := text[m[4]:m[5]]
		if seg == "." || seg == ".." {
			continue // 相对路径段，不是用户名
		}
		b.WriteString(text[prev:m[4]])
		b.WriteString(tagged("USERNAME", seg, key))
		prev = m[5]
		replaced = true
	}
	if !replaced {
		return text
	}
	b.WriteString(text[prev:])
	return b.String()
}

// sshTargetRe 匹配 ssh/scp 等命令目标里的 user@host（邮箱规则刻意
// 跳过 ssh 行，这里补上——user 是登录名、host 是基础设施主机名，
// 都是真实泄漏点）。
var sshTargetRe = regexp.MustCompile(`\b([a-z0-9._-]{1,32})@([a-z0-9][a-z0-9.-]{0,62})`)

// replaceSSHTargets 把 ssh 命令行里的 user@host 分别换成
// [USERNAME_xxxxxx]@[HOSTNAME_xxxxxx]。host 段遇到端口（:22）或
// 路径（:/file）自然截停。配合恢复通道：模型写出的 ssh 命令参数
// 在执行前换回真值，命令照跑。
func replaceSSHTargets(text string, key []byte) string {
	idx := sshTargetRe.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		return text
	}
	var b strings.Builder
	prev := 0
	replaced := false
	for _, m := range idx {
		if !isInSSHCommandContext(text, m[0]) {
			continue
		}
		// group 1: user；group 2: host。逐段替换，间隔字符原样保留。
		b.WriteString(text[prev:m[2]])
		b.WriteString(tagged("USERNAME", text[m[2]:m[3]], key))
		b.WriteString(text[m[3]:m[4]])
		b.WriteString(tagged("HOSTNAME", text[m[4]:m[5]], key))
		prev = m[5]
		replaced = true
	}
	if !replaced {
		return text
	}
	b.WriteString(text[prev:])
	return b.String()
}
