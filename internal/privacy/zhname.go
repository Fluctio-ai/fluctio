package privacy

import (
	_ "embed"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 中文姓名识别。机制移植自 yetone/magpie (MIT, internal/redact/zhname.go):
// 姓氏表 + 名 1-2 字 + 上下文触发 (角色/前导动词/后随称谓), 反误伤靠
// 普通词表 (zhWords)、jieba 派生词典 (zhdict.txt, MIT, Sun Junyi) 与
// 名词后缀。与 magpie 的差异: 我们没有本地 NER 模型, 规则负责全部
// 姓名 (magpie 有模型时规则只补昵称); 输出走 tagged("PERSON") 进入
// 恢复台账。magpie 的模型接边逻辑 (zhTrim / person join) 相应省略或
// 简化为候选间联动 (joined)。

// zhSurnames are the common single-character surnames.
const zhSurnames = "王李张刘陈杨黄赵吴周徐孙马朱胡郭何高林罗郑梁谢宋唐许韩冯邓曹彭曾肖萧田董袁潘于蒋蔡余杜叶程苏魏吕丁任沈姚卢姜崔钟谭陆汪范金石廖贾夏韦付傅方白邹孟熊秦邱江尹薛闫段雷侯龙史陶黎贺顾毛郝龚邵万钱严覃武戴莫孔向汤常温康施文牛樊葛邢安齐易乔伍庞颜倪庄聂章鲁岳翟殷詹申欧耿关兰焦俞左柳甘祝包宁尚符舒阮柯纪梅童凌毕单季裴霍涂成苗谷盛曲翁冉骆蓝路游辛靳管柴蒙鲍华喻祁蒲房滕屈饶解牟艾尤阳时穆农司卓古吉缪简车项连芦麦褚娄窦戚岑景党宫费卜冷晏席卫米柏宗瞿桂全佟应臧闵苟邬边卞姬师仇栾隋商刁沙荣巫寇桑郎甄丛仲虞敖巩佘池查麻苑迟邝"

// zhCompound are the two-character surnames.
var zhCompound = []string{"欧阳", "司马", "上官", "诸葛", "东方", "皇甫", "尉迟", "公孙", "慕容", "令狐", "司徒", "夏侯", "长孙", "宇文", "轩辕", "端木", "独孤", "南宫", "西门", "钟离", "呼延", "申屠", "闻人", "澹台", "拓跋", "万俟"}

// zhNickSurnames are those 小 and 老 go before for a name (小王, 老李).
const zhNickSurnames = "王李张刘陈杨黄赵吴周徐孙马朱胡郭何高林罗郑梁谢宋唐许韩冯邓曹彭曾肖田董袁潘蒋蔡杜叶程苏魏吕丁沈姚卢姜崔钟谭陆汪范金廖贾夏方邹孟熊秦江薛段雷侯史陶贺顾毛郝龚邵钱严武戴孔汤"

// zhNotGiven can't be a given name's character: words a sentence goes on
// with after a name.
const zhNotGiven = "的了在是和与跟同说他她它我你您们这那个不也都就还把被给到对从向为以等及或并而吗呢吧啊去来有没要会能让请已将再又很更最先后上下里外时年日号发打回写问讲叫找用做看想知道得地着过之其此每各某该所反表认提负"

// zhWords begin with a surname but are words, not names: 文案 in
// "改一下文案" is no one. Each is two characters, so a candidate's first
// two are looked up (zhWord). 毛利 is a gross margin and a Japanese
// surname too (毛利兰): left out either way, a word list can't tell them.
var zhWords = runes2(
	"张量", "张开", "张贴", "高度", "高效", "高级", "高速", "高亮", "高手", "高兴", "王国", "王者",
	"黄河", "黄色", "方法", "方向", "方式", "方案", "方便", "方面", "程序", "程度", "温度", "白色",
	"金额", "金融", "江湖", "石头", "马上", "周期", "周末", "周围", "周边", "文件", "文本", "文档",
	"文字", "文章", "余额", "安全", "安装", "安排", "钱包", "叶子", "毛病", "毛利", "毛重", "毛坯", "毛巾", "毛衣", "万一", "雷达", "宋体",
	"常量", "常用", "常见", "向量", "向上", "全部", "全局", "全面", "史上", "易用", "武器", "时间",
	"时候", "明天", "明确", "明白", "任何", "任务", "任意", "华为", "管理", "简单", "简介", "范围",
	"许多", "许可", "包括", "包含", "关于", "关系", "关闭", "连接", "成功", "成为", "应用", "应该",
	"单位", "单元", "单独", "司机", "车辆", "车子", "路径", "路由", "林业", "龙头", "柏林", "米饭",
	"宁可", "师傅", "老师", "老板", "老婆", "老公", "老家", "小时", "小心", "小说", "小姐", "小孩",
	"小组", "周一", "周二", "周三", "周四", "周五", "周六", "周日", "周天", "文案", "文风", "文化",
	"文学", "文明", "文献", "文法", "文具", "方块", "方针", "方程", "方框", "关键", "关注", "关联",
	"关心", "关掉", "项目", "成本", "成员", "成果", "成绩", "成熟", "成就", "解决", "解析", "解释",
	"解码", "段落", "章节", "颜色", "蓝色", "查询", "查看", "查找", "单词", "单个", "单击", "包装",
	"包名", "时区", "时长", "时段", "时刻", "时代", "季度", "严格", "严重", "曾经", "麻烦", "安卓",
	"谷歌", "常规", "常数", "边框", "边界", "边缘", "付费", "付款", "路线", "简洁", "简历", "简化",
	"范例", "游戏", "游客", "商品", "商店", "商业", "左边", "左侧", "焦点", "毕竟", "尚未",
	"陆续", "连续", "连线", "林间", "林区", "谢谢", "顾客", "顾问", "房间", "房子", "高端", "高层",
	"古代", "古典", "农业", "司法", "施工", "卫生", "荣誉", "宗旨", "齐全", "凌晨", "冷却", "蒙版",
	"曲线", "黄金", "白天", "白板", "白名", "金钱", "石油", "夏天", "夏季", "唐朝", "宋朝", "秦朝", "申请",
	"符号", "景色", "费用", "舒服", "辛苦", "纪律", "席位", "车间", "管道", "何时", "何况", "于是",
	"余下", "杜绝", "罗列", "程式", "叶片", "涂层", "沙发", "池子", "巩固", "迟到", "仲裁", "钟点",
	"钟表", "向导", "史诗", "易懂", "马路", "马虎", "汤汁", "朱红", "阳光", "任职", "董事",
	"边距", "边栏", "尚需",
)

// zhDict are more two-character words that start with a surname, from a
// dictionary (zhdict.txt says which and how): 边界, 应该, 尚可, 关心.
//
//go:embed zhdict.txt
var zhDictText string

var zhDict = func() map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(zhDictText, "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		for _, w := range strings.Fields(l) {
			m[w] = true
		}
	}
	return m
}()

// zhColours are the colour characters: a word of them alone (黄绿, 红蓝)
// or one that ends in 色 (黄绿色, 金黄色, 红色) is a colour, no name.
// 青 is left out, a given name's as often (王青).
const zhColours = "红橙黄绿蓝紫黑白灰褐棕粉赤"

// zhNounEnds are what makes a word a longer one (关键 → 关键词, 边界 →
// 边界线, 白名 → 白名单, 毛利 → 毛利率), and no given name ends with.
const zhNounEnds = "词单字性率值码型键框栏库器类表量度点线层期区图版式页数论员价计费件名夹帧戳"

// zhWordShaped says v, a Chinese name the cues took, is an ordinary word:
// a colour (黄绿色), a word that starts with a surname (zhWords, zhDict:
// 边界, 尚可), or one of those with what makes a longer word after it
// (关键词, 关键字; 应用层).
func zhWordShaped(v string) bool {
	rs := []rune(v)
	if len(rs) < 2 {
		return false
	}
	colour := true
	for _, r := range rs {
		if !unicode.Is(unicode.Han, r) {
			return false
		}
		colour = colour && strings.ContainsRune(zhColours, r)
	}
	if colour || rs[len(rs)-1] == '色' {
		return true
	}
	two := string(rs[:2])
	switch len(rs) {
	case 2:
		return zhWords[two] || zhDict[two]
	case 3:
		return (zhWords[two] || zhDict[two]) && strings.ContainsRune(zhNounEnds, rs[2])
	}
	return false
}

// zhWordNotName says s[a:b], taken for a Chinese name, is an ordinary
// word there (zhWordShaped): unless a role before it or a title after it
// says a person is meant (客户方圆, 我叫高歌, 江山老师), where a name
// that is also a word is masked.
func zhWordNotName(s string, a, b int) bool {
	return zhWordShaped(s[a:b]) && !endsAny(s[:a], zhRoles, true) && !startsAny(s[b:], zhTitles)
}

// zhRoles before a name say it is one: 客户{{PERSON}}, 联系人：{{PERSON}},
// 我叫{{PERSON}}, 提交者是{{PERSON}}.
var zhRoles = []string{"客户", "联系人", "作者", "提交者", "审核人", "审批人", "负责人", "经办人", "收件人", "发件人", "收货人", "申请人", "报告人", "同事", "姓名", "名字", "署名", "签名", "我叫", "他叫", "她叫", "名叫", "叫做", "尊敬的", "亲爱的", "致", "@"}

// zhBefore are words that take a person after them (给{{PERSON}}, 问{{PERSON}}), so
// a name after them is one where something after it says so too.
var zhBefore = []string{"给", "找", "问", "让", "告诉", "通知", "联系", "抄送", "转给", "交给", "一下", "叫", "和", "与", "跟", "说", "请", "我是", "他是", "她是"}

// zhAfter are words that follow a person ({{PERSON}}先生, {{PERSON}}说, {{PERSON}}反馈).
var zhAfter = []string{"先生", "女士", "小姐", "老师", "同学", "同志", "经理", "总监", "主任", "师傅", "医生", "律师", "教授", "博士", "说", "讲", "表示", "认为", "反馈", "提到", "提交", "负责", "告诉", "回复", "发", "打", "明天", "今天", "昨天", "已经", "他", "她"}

// zhAnyAfter are those of zhAfter that follow any subject, a thing as
// well as a person (文案已经改好了, 文案明天给你, 文档提交): at a
// sentence's start, with nothing before it that says a person, they say
// nothing.
var zhAnyAfter = []string{"提交", "负责", "回复", "明天", "今天", "昨天", "已经", "他", "她"}

// zhAskVerbs take a person before 一下 (问一下{{PERSON}}, 联系一下{{PERSON}}); after
// any other verb what 一下 is followed by is a thing (改一下文案,
// 检查一下文档).
var zhAskVerbs = []string{"问", "找", "叫", "喊", "催", "请", "联系", "通知", "告诉", "提醒", "回复", "答复", "拜访", "采访", "抄送", "转告", "感谢", "谢谢", "艾特", "@"}

// zhTitles are what follows a name and isn't part of it.
var zhTitles = []string{"先生", "女士", "小姐", "老师", "同学", "同志", "经理", "总监", "主任", "师傅", "医生", "律师", "教授", "博士"}

var (
	isSurname, isNick, notGiven = runes(zhSurnames), runes(zhNickSurnames), runes(zhNotGiven)
	mayStart                    = runes(zhSurnames + zhFirsts() + "小老")
)

type zhSpan struct{ start, end int }

// replaceZhNames 把 s 中上下文确认的中文姓名换成 [PERSON_xxxxxx]。
// 无本地 NER 模型, 规则负责全部姓名 (含昵称); 普通词表 + 词典 + 名词
// 后缀防误伤, 光杆姓氏/无上下文姓名不脱 (王国的军队, 张开的括号)。
func replaceZhNames(s string, key []byte) string {
	if !hasHan(s) {
		return s
	}
	spans := zhNameSpans(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp.start])
		b.WriteString(tagged("PERSON", s[sp.start:sp.end], key))
		prev = sp.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// zhNameSpans 是 s 中上下文确认的姓名 span。候选经 zhName 逐个判定;
// 与已确认姓名以 和/与/跟/及/、 相连的候选联动确认为姓名
// (李娜和刘洋, 只有刘洋带后随词)。
func zhNameSpans(s string) []zhSpan {
	type cand struct {
		zhSpan
		ok bool
	}
	var cs []cand
	for i := 0; i < len(s); {
		a, b, ok := zhName(s, i)
		if b == 0 {
			_, n := utf8.DecodeRuneInString(s[i:])
			i += n
			continue
		}
		i = b
		if zhWordNotName(s, a, b) {
			continue // 普通词: 文案, 边界
		}
		cs = append(cs, cand{zhSpan{a, b}, ok})
	}
	starts, ends := map[int]bool{}, map[int]bool{}
	for _, c := range cs {
		if c.ok {
			starts[c.start], ends[c.end] = true, true
		}
	}
	for grew := true; grew; {
		grew = false
		for i, c := range cs {
			if !c.ok && joined(s, c.zhSpan, starts, ends) {
				cs[i].ok, grew = true, true
				starts[c.start], ends[c.end] = true, true
			}
		}
	}
	var out []zhSpan
	for _, c := range cs {
		if c.ok {
			out = append(out, c.zhSpan)
		}
	}
	return out
}

// joined says only 和, 与, 跟, 及 or 、 is between c and a confirmed
// name, one that ends in ends or starts in starts.
func joined(s string, c zhSpan, starts, ends map[int]bool) bool {
	for _, j := range []string{"和", "与", "跟", "及", "、"} {
		if strings.HasPrefix(s[c.end:], j) && starts[c.end+len(j)] ||
			strings.HasSuffix(s[:c.start], j) && ends[c.start-len(j)] {
			return true
		}
	}
	return false
}

// zhName is the name that may start at i: its span, and whether what is
// around it says it is one (b is 0 where none can start there).
func zhName(s string, i int) (a, b int, ok bool) {
	rest := s[i:]
	if r, _ := utf8.DecodeRuneInString(rest); !mayStart[r] {
		return 0, 0, false
	}
	sur := 0
	for _, c := range zhCompound {
		if strings.HasPrefix(rest, c) {
			sur = len(c)
			break
		}
	}
	nick := false
	if sur == 0 {
		r, n := utf8.DecodeRuneInString(rest)
		switch {
		case (r == '小' || r == '老') && len(rest) > n:
			if r2, n2 := utf8.DecodeRuneInString(rest[n:]); isNick[r2] {
				sur, nick = n+n2, true
			}
		case isSurname[r]:
			sur = n
		}
	}
	if sur == 0 {
		return 0, 0, false
	}
	// the given name: none after 小王, else one or two characters
	ends := []int{}
	if nick {
		ends = append(ends, i+sur)
	} else {
		at := i + sur
		for k := 0; k < 2 && at < len(s); k++ {
			r, n := utf8.DecodeRuneInString(s[at:])
			if !unicode.Is(unicode.Han, r) || notGiven[r] {
				break
			}
			at += n
			ends = append(ends, at)
		}
	}
	if len(ends) == 0 {
		return 0, 0, false
	}
	if zhWord(rest) {
		return 0, 0, false
	}
	// a Han character before it that is no cue makes it part of a word
	// (建国路), unless a cue ends there
	before := s[:i]
	role, verb := endsAny(before, zhRoles, true), endsAny(before, zhBefore, false)
	// 和 and 与 join like to like: a person after one where a person is
	// before it (吴敏和郑浩, 我和张伟), not 判断和边界
	for _, j := range []string{"和", "与"} {
		if t, ok := strings.CutSuffix(before, j); ok && verb {
			verb = personBefore(t)
		}
	}
	// 一下 takes a person only after a verb that asks one (问一下张伟),
	// not 改一下文案
	if t, ok := strings.CutSuffix(before, "一下"); ok && verb {
		verb = endsAny(t, zhAskVerbs, false)
	}
	start := sentenceStart(before)
	if !role && !verb && !start {
		return 0, 0, false
	}
	// a surname and a title is a person (孙经理, 邓总)
	if !nick {
		for _, t := range append(zhTitles, "总") {
			if rest := s[i+sur:]; strings.HasPrefix(rest, t) && !(t == "总" && zhTotal(rest[len(t):])) {
				return i, i + sur + len(t), true
			}
		}
	}
	// the longest that ends where something after it says so
	for k := len(ends) - 1; k >= 0; k-- {
		e := ends[k]
		if after := s[e:]; startsAny(after, zhAfter) && !wordAfter(after) && (role || verb || !startsAny(after, zhAnyAfter)) {
			return i, e, true
		} else if (role || verb) && endsWord(after) {
			return i, e, true
		}
	}
	if role {
		return i, ends[len(ends)-1], true
	}
	return i, ends[len(ends)-1], false
}

// zhTotal says 总 before s is a word's (总是, 总结, 总共), not a title.
func zhTotal(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return strings.ContainsRune("是结共体之数计线量部归算会得要", r)
}

// endsAny says s ends with one of ws; for a role, with one followed by
// 是, 为, a colon or spaces.
func endsAny(s string, ws []string, role bool) bool {
	if role {
		s = strings.TrimRight(s, " \t")
		for _, sep := range []string{"：", ":", "是", "为"} {
			if t, ok := strings.CutSuffix(s, sep); ok {
				s = strings.TrimRight(t, " \t")
				break
			}
		}
	}
	for _, w := range ws {
		if strings.HasSuffix(s, w) {
			return true
		}
	}
	return false
}

func startsAny(s string, ws []string) bool {
	for _, w := range ws {
		if strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// endsWord says a word ends where s starts: s is empty, or starts with no
// Han character.
func endsWord(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return s == "" || !unicode.Is(unicode.Han, r)
}

// sentenceStart says a name may start after s: s is empty or ends in
// punctuation, spaces after it or none. A space after a word or code
// (`gh pr view` 易触发) is inside a sentence.
func sentenceStart(s string) bool {
	s = strings.TrimRight(s, " \t　")
	if s == "" {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsPunct(r) || unicode.IsSpace(r)
}

// wordAfter says the 发 or 打 that s starts with begins a word (发现,
// 打开), not the verb a person does (发了个消息, 打给他); and so do
// 说明 and 讲解 (文案说明要简短), but for 说明天 (张伟说明天来).
func wordAfter(s string) bool {
	for _, w := range []string{"说明", "说法", "讲解", "讲座", "讲义", "讲究"} {
		if strings.HasPrefix(s, w) {
			return !strings.HasPrefix(s, "说明天")
		}
	}
	for _, v := range []string{"发", "打"} {
		if t, ok := strings.CutPrefix(s, v); ok {
			r, _ := utf8.DecodeRuneInString(t)
			return unicode.Is(unicode.Han, r) && !strings.ContainsRune("了个给过一", r)
		}
	}
	return false
}

// personBefore says s ends with a pronoun or what may be a name: a
// surname and one to three characters, no word of zhWords.
func personBefore(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	if strings.ContainsRune("我你您他她们", r) {
		return true
	}
	rs := []rune(s)
	for n := 2; n <= 4 && n <= len(rs); n++ {
		u := string(rs[len(rs)-n:])
		if !mayStart[rs[len(rs)-n]] || !hasHan(u) {
			continue
		}
		if !zhWord(u) {
			return true
		}
	}
	return false
}

func zhWord(s string) bool {
	_, n := utf8.DecodeRuneInString(s)
	_, n2 := utf8.DecodeRuneInString(s[n:])
	return zhWords[s[:n+n2]]
}

func runes2(ws ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range ws {
		m[w] = true
	}
	return m
}

func runes(s string) map[rune]bool {
	m := map[rune]bool{}
	for _, r := range s {
		m[r] = true
	}
	return m
}

func zhFirsts() string {
	var b strings.Builder
	for _, c := range zhCompound {
		r, _ := utf8.DecodeRuneInString(c)
		b.WriteRune(r)
	}
	return b.String()
}

func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x3400 && unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
