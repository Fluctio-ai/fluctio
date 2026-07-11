package channels

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

func TestFeishuSendMediaItemUploadsAndSendsFile(t *testing.T) {
	var uploaded, sent bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/im/v1/files":
			uploaded = true
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("file_type") != "stream" || r.FormValue("file_name") != "report.pdf" {
				t.Fatalf("unexpected form: %#v", r.Form)
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if string(data) != "pdf-data" {
				t.Fatalf("uploaded data = %q", data)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"file_key":"fk_1"}}`))
		case "/open-apis/im/v1/messages":
			sent = true
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["msg_type"] != "file" || payload["receive_id"] != "chat_1" || payload["content"] != `{"file_key":"fk_1"}` {
				t.Fatalf("payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	l := &Feishu{httpClient: server.Client(), apiBaseURL: server.URL}
	err := l.sendMediaItem("token", "chat_1", bus.MediaItem{Filename: "report.pdf", ContentType: "application/pdf", Bytes: []byte("pdf-data")})
	if err != nil {
		t.Fatal(err)
	}
	if !uploaded || !sent {
		t.Fatalf("uploaded=%v sent=%v", uploaded, sent)
	}
}
