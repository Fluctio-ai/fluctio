package kb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStripHTMLToText verifies the fallback path preserves paragraph/heading/
// list structure (block-level tags → newlines) and fully decodes entities —
// the exact regression that motivated this change.
func TestStripHTMLToText(t *testing.T) {
	in := `<html><body><nav>Home About</nav><h1>Title</h1>` +
		`<p>Para one with &amp; ampersand and &#39;quote&#39;.</p>` +
		`<p>Para two.</p><ul><li>Alpha</li><li>Beta</li></ul></body></html>`
	text := stripHTMLToText(in)

	if !strings.Contains(text, "Para one") || !strings.Contains(text, "Para two") {
		t.Fatalf("missing paragraphs: %q", text)
	}
	// Paragraphs must NOT run together — a newline must separate them.
	i1, i2 := strings.Index(text, "Para one"), strings.Index(text, "Para two")
	if strings.Count(text[i1:i2], "\n") == 0 {
		t.Errorf("paragraphs run together (no newline between):\n%s", text)
	}
	// List items keep separate lines too.
	if strings.Contains(text, "Alpha") && strings.Contains(text, "Beta") {
		ja, jb := strings.Index(text, "Alpha"), strings.Index(text, "Beta")
		if strings.Count(text[ja:jb], "\n") == 0 {
			t.Errorf("list items run together:\n%s", text)
		}
	}
	// Entities fully decoded.
	if !strings.Contains(text, "&") || !strings.Contains(text, "'quote'") {
		t.Errorf("entity not decoded: %q", text)
	}
}

// TestFetchURLContentExtractsArticle drives FetchURLContent against a local
// httptest server serving a page with nav + article + footer, and verifies
// the article text is returned with paragraph structure intact (newlines
// between paragraphs, not one fused blob).
func TestFetchURLContentExtractsArticle(t *testing.T) {
	// kbFetchClient applies the SSRF dial guards, which block the
	// 127.0.0.1 loopback httptest server — opt into the documented
	// loopback escape hatch for this test.
	t.Setenv("FLUCTIO_ALLOW_UNSAFE_LOOPBACK_FETCH", "1")
	const page = `<!DOCTYPE html><html><head><title>Test Article</title></head><body>
<nav>Home About Contact Login</nav>
<article>
<h1>Real Headline</h1>
<p>This is the first real paragraph about widgets and their uses. It has more than enough words for readability to score it highly as main content worth extracting.</p>
<p>A second paragraph elaborates on the widget theme with additional sentences and meaningful context for the reader to follow along.</p>
<p>The third paragraph continues the discussion with yet more prose about widgets, ensuring the article body dominates the page over navigation chrome.</p>
</article>
<aside>Sidebar promo buy now discount</aside>
<footer>Copyright notice links terms privacy</footer>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	title, body, err := FetchURLContent(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchURLContent: %v", err)
	}
	if title == "" {
		t.Errorf("empty title")
	}
	if !strings.Contains(body, "first real paragraph") {
		t.Errorf("body missing article text: %q", body)
	}
	// Structure must survive — paragraphs separated by newlines, not glued.
	if strings.Count(body, "\n") == 0 {
		t.Errorf("body has no newlines (structure lost): %q", body)
	}
}
