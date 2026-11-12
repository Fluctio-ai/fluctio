package privacy

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// 用户自定义敏感词（RedactWords，借鉴 yetone/magpie, MIT）与
// 用户名/主机名规则。含 CJK 的词按子串匹配（中文无词边界）；纯
// ASCII 的词按整词边界 + 大小写折叠匹配，避免 cat 咬住 concatenate。

// wordsReCache 按词表内容缓存编译好的 alternation。词表每次 PUT 都
// 全量重写，实际种类极少，缓存避免每次 scrub 重新编译。
var wordsReCache sync.Map // key: strings.Join(words, "\x00") → *regexp.Regexp

// wordsPattern 把词表编译成一条 alternation：ASCII 词带 \b 整词边界，
// CJK 词裸匹配；长度降序保证 leftmost-first 下长词优先。
func wordsPattern(words []string) *regexp.Regexp {
	key := strings.Join(words, "\x00")
	if v, ok := wordsReCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	sorted := slices.Clone(words)
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	var alt []string
	for _, w := range sorted {
		if w == "" {
			continue
		}
		if isASCIIWord(w) {
			alt = append(alt, `\b`+regexp.QuoteMeta(w)+`\b`)
		} else {
			alt = append(alt, regexp.QuoteMeta(w))
		}
	}
	re := regexp.MustCompile(`(?i)(?:` + strings.Join(alt, "|") + `)`)
	wordsReCache.Store(key, re)
	return re
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

// replaceWords 把 text 中命中用户敏感词的部分换成 [WORD_xxxxxx]。
func replaceWords(text string, words []string, key []byte) string {
	if len(words) == 0 {
		return text
	}
	return wordsPattern(words).ReplaceAllStringFunc(text, func(m string) string {
		return tagged("WORD", m, key)
	})
}

// homeUserRe 匹配系统家目录路径的用户名段：/home/<u>、/Users/<u>
// （macOS）、C:\Users\<u>（Windows）。只脱用户名段，路径其余部分
// 保留可读性。
var homeUserRe = regexp.MustCompile(`(?i)([a-z]:\\users\\|/home/|/users/)([^/\\\s]{1,32})`)

// replaceHomeUsers 把家目录路径中的用户名换成 [USERNAME_xxxxxx]。
func replaceHomeUsers(text string, key []byte) string {
	return replaceGroups(text, homeUserRe, []int{2}, func(v string, _ int) string {
		return tagged("USERNAME", v, key)
	}, func(t string, s, e int) bool {
		m := t[s:e]
		if i := strings.LastIndexAny(m, "/\\"); i >= 0 {
			m = m[i+1:]
		}
		return m != "." && m != ".."
	})
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
	return replaceGroups(text, sshTargetRe, []int{1, 2}, func(v string, g int) string {
		if g == 1 {
			return tagged("USERNAME", v, key)
		}
		return tagged("HOSTNAME", v, key)
	}, func(t string, s, _ int) bool {
		return isInSSHCommandContext(t, s)
	})
}
