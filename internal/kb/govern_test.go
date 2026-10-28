package kb

import (
	"context"
	"strings"
	"testing"
)

func TestGovernBasePackFormattingNeutral(t *testing.T) {
	in := "第一段内容。​中间零宽\r\n\r\n\r\n\r\n第二段   \n第三段\t\n\n\n\n\n第四段"
	out, hits := GovernContent(in, "text")

	if strings.Contains(out, "​") {
		t.Errorf("zero-width char survived: %q", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("blank run survived: %q", out)
	}
	if strings.Contains(out, "   ") || strings.Contains(out, "\t") {
		t.Errorf("trailing whitespace survived: %q", out)
	}
	for _, body := range []string{"第一段内容。", "中间零宽", "第二段", "第三段", "第四段"} {
		if !strings.Contains(out, body) {
			t.Errorf("real content lost %q: %q", body, out)
		}
	}
	if hits["zero_width"] != 1 || hits["trailing_ws"] == 0 || hits["blank_run"] == 0 {
		t.Errorf("hit counts wrong: %+v", hits)
	}
}

func TestGovernWebPackRemovesBoilerplateLines(t *testing.T) {
	in := strings.Join([]string{
		"We use cookies to improve your experience",
		"本文约 1500 字，阅读需要 6 分钟",
		"真正的正文第一段，讲了一件重要的事。",
		"点击上方蓝字关注公众号，获取最新资讯",
		"长按二维码识别，加微信进群",
		"喜欢本文，欢迎点击在看支持一下",
		"阅读原文",
		"真正的正文第二段，还有后续内容。",
	}, "\n")
	out, hits := GovernContent(in, "url")

	for _, junk := range []string{"cookies", "本文约", "关注公众号", "二维码", "点击在看", "阅读原文"} {
		if strings.Contains(out, junk) {
			t.Errorf("boilerplate %q survived: %q", junk, out)
		}
	}
	for _, body := range []string{"真正的正文第一段", "真正的正文第二段"} {
		if !strings.Contains(out, body) {
			t.Errorf("real content lost %q: %q", body, out)
		}
	}
	for _, name := range []string{"read_time", "read_original", "qr_cta", "follow_cta", "share_cta", "cookie_banner"} {
		if hits[name] != 1 {
			t.Errorf("rule %s hit %d times, want 1 (hits %+v)", name, hits[name], hits)
		}
	}
}

func TestGovernWebPackOnlyForURLSources(t *testing.T) {
	in := "正文。\n点击上方蓝字关注公众号，获取最新资讯\n更多正文。"
	out, _ := GovernContent(in, "text")
	if !strings.Contains(out, "关注公众号") {
		t.Errorf("web rules must not fire for text sources: %q", out)
	}
	out, _ = GovernContent(in, "url")
	if strings.Contains(out, "关注公众号") {
		t.Errorf("web rules must fire for url sources: %q", out)
	}
}

func TestGovernKeepsMentionsThatAreNotBoilerplate(t *testing.T) {
	in := "长按下文查看详情\n关注我最新动态\n扫码加我微信聊业务"
	out, _ := GovernContent(in, "url")
	// None of these lines IS boilerplate: no 二维码 on the QR line, no
	// 公众号/订阅号 on the follow line. They must survive.
	if out != in {
		t.Errorf("non-boilerplate lines were removed:\n in=%q\nout=%q", in, out)
	}
}

// TestIngestTextAppliesGovernance verifies the cleaned content is what gets
// chunked and stored — boilerplate must not reach kb_entries.
func TestIngestTextAppliesGovernance(t *testing.T) {
	db := setupKBVectorTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ks := NewKBStore(db, "sqlite")
	content := strings.Join([]string{
		"文章正文：重庆小升初政策整理。",
		"本文约 2000 字，阅读需要 8 分钟",
		"第一段实质内容在这里。",
		"扫描上方二维码关注公众号领资料",
		"第二段实质内容在这里。",
	}, "\n")
	if _, err := ks.IngestText(ctx, "a1", "测试文章", content, "url", "https://mp.weixin.qq.com/s/xyz"); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var joined string
	if err := db.QueryRowContext(ctx,
		`SELECT GROUP_CONCAT(ke.content, '') FROM kb_entries ke JOIN kb_sources s ON s.id = ke.source_id WHERE s.agent_id = 'a1'`).Scan(&joined); err != nil {
		t.Fatalf("read entries: %v", err)
	}
	for _, junk := range []string{"本文约", "二维码", "公众号"} {
		if strings.Contains(joined, junk) {
			t.Errorf("boilerplate %q reached kb_entries: %q", junk, joined)
		}
	}
	if !strings.Contains(joined, "第一段实质内容") || !strings.Contains(joined, "第二段实质内容") {
		t.Errorf("real content missing from kb_entries: %q", joined)
	}
}
