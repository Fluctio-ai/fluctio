package privacy

import (
	"testing"
)

// TestKeyPerAgentIsolation: 同一值在不同 agent 密钥下指纹不同
// (厂商无法跨 agent 关联同一 PII), 同一 agent 内稳定。
func TestKeyPerAgentIsolation(t *testing.T) {
	v := "13900139000"
	ka, kb := KeyFor("agt_a"), KeyFor("agt_b")
	pa := ScrubWith(v, Options{Key: ka})
	pb := ScrubWith(v, Options{Key: kb})
	if pa == pb {
		t.Fatalf("不同 agent 密钥产生相同指纹: %q", pa)
	}
	if pa != ScrubWith(v, Options{Key: ka}) {
		t.Fatal("同一 agent 密钥两次 scrub 指纹不同")
	}
	global := ScrubWith(v, Options{})
	if global == pa {
		t.Fatal("全局密钥与 agent 密钥指纹应不同")
	}
}

// TestScrubWords: 用户自定义敏感词。CJK 子串匹配; ASCII 整词 +
// 大小写折叠; 长词优先; 恢复通道可还原。
func TestScrubWords(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"cjk word", "启动夜莺项目计划", "启动[WORD]计划"},
		{"cjk substring", "夜莺项目V2上线", "[WORD]V2上线"},
		{"ascii whole word", "cat is cute", "[WORD] is cute"},
		{"ascii case fold", "the CAT sat", "the [WORD] sat"},
		{"ascii no substring", "concatenate stays", "concatenate stays"},
		{"longer first", "夜莺项目 vs 夜莺", "[WORD] vs [WORD]"},
		{"no word config", " concatenate ", " concatenate "},
	}
	words := []string{"cat", "夜莺项目", "夜莺"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "no word config" {
				if got := ScrubWith(tt.in, Options{}); got != tt.in {
					t.Fatalf("无词表时应原样: %q", got)
				}
				return
			}
			got := ScrubWith(tt.in, Options{Words: words})
			if !wantScrubRe(tt.want).MatchString(got) {
				t.Fatalf("ScrubWith(Words)(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	// 恢复通道闭环
	in := "联系夜莺项目组"
	scrubbed := ScrubWith(in, Options{Words: []string{"夜莺项目"}})
	if got := Restore(scrubbed); got != in {
		t.Fatalf("Words 恢复失败: %q -> %q -> %q", in, scrubbed, got)
	}
}

// TestScrubZhNames: 中文姓名。上下文确认才脱 (角色/前导动词/后随
// 称谓/句首); 普通词表 / 颜色 / 名词后缀防误伤。
func TestScrubZhNames(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"role before", "客户张伟说需求变了", "客户[PERSON]说需求变了"},
		{"title after", "请联系李娜女士", "请联系[PERSON]女士"},
		{"wo jiao", "自我介绍一下, 我叫王建国", "自我介绍一下, 我叫[PERSON]"},
		{"compound surname", "提交者是欧阳锋", "提交者是[PERSON]"},
		{"verb+after", "问一下刘洋明天来吗", "问一下[PERSON]明天来吗"},
		{"joined", "客户李娜和刘洋都来了", "客户[PERSON]和[PERSON]都来了"},
		{"nickname role", "客户小王", "客户[PERSON]"},
		{"bare name untouched", "张伟今天没来", "张伟今天没来"},
		{"bare pair untouched", "李娜和刘洋都来了", "李娜和刘洋都来了"},
		{"word kingdom", "王国的军队前进", "王国的军队前进"},
		{"word open", "注意张开的括号", "注意张开的括号"},
		{"word height", "高度集中精神", "高度集中精神"},
		{"colour", "喜欢黄绿色", "喜欢黄绿色"},
		{"noun end", "点一下关键词", "点一下关键词"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scrub(tt.in)
			if !wantScrubRe(tt.want).MatchString(got) {
				t.Fatalf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestScrubHomeUsers: 家目录用户名。只脱用户名段, 路径其余保留。
func TestScrubHomeUsers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"linux home", "cat /home/mumu/.bashrc", "cat /home/[USERNAME]/.bashrc"},
		{"mac users", "open /Users/mumu/doc.txt", "open /Users/[USERNAME]/doc.txt"},
		{"mac root", "/var/root/x 不是家目录", "/var/root/x 不是家目录"},
		{"relative dotdot", "cd /home/../etc", "cd /home/../etc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scrub(tt.in); !wantScrubRe(tt.want).MatchString(got) {
				t.Fatalf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	in := "cat /home/mumu/.bashrc"
	if got := Restore(Scrub(in)); got != in {
		t.Fatalf("USERNAME 恢复失败: %q", got)
	}
}

// TestScrubSSHTargets: ssh 命令行的 user@host 分标签脱敏;
// git@host:path (git 协议) 不受影响。
func TestScrubSSHTargets(t *testing.T) {
	ssh := Scrub("ssh root@prod-db-1 uptime")
	if !wantScrubRe("ssh [USERNAME]@[HOSTNAME] uptime").MatchString(ssh) {
		t.Fatalf("ssh 目标未脱: %q", ssh)
	}
	git := Scrub("git clone git@github.com:user/repo.git")
	if git != "git clone git@github.com:user/repo.git" {
		t.Fatalf("git 协议被误脱: %q", git)
	}
	// 恢复闭环
	in := "ssh deploy@prod-1 "
	if got := Restore(Scrub(in)); got != in {
		t.Fatalf("ssh 目标恢复失败: %q", got)
	}
}

// TestZhNameRestoreRoundTrip: 姓名占位符经恢复通道还原。
func TestZhNameRestoreRoundTrip(t *testing.T) {
	in := "客户康风先生下周到"
	scrubbed := Scrub(in)
	if scrubbed == in {
		t.Fatal("姓名未脱敏")
	}
	if got := Restore(scrubbed); got != in {
		t.Fatalf("PERSON 恢复失败: %q -> %q -> %q", in, scrubbed, got)
	}
}
