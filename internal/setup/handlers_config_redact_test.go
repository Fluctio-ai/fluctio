package setup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/config"
	"github.com/fluctio-ai/fluctio/internal/scope"
)

// Server-level guard for the config redaction work: with live credentials
// seeded across every flagged namespace, the GET responses must not carry
// any plaintext secret. Replaces the "boot the gateway and curl it"
// acceptance smoke — same handlers, same store wiring, hermetic.
func TestGetConfigResponsesRedactCredentials(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, _ := newAuthTestServer(t, ctx)

	const (
		provKey   = "sk-live-0123456789abcdef-REAL"
		e2bKey    = "e2b_live_0123456789abcdef"
		boxKey    = "boxlite_live_0123456789abcdef"
		hooksTok  = "hooks-token-0123456789abcdef"
		botTok    = "777777:AAAbbbCCC-real-token"
		acctTok   = "acct-token-0123456789abcdef"
		qqSecret  = "qq-secret-0123456789abcdef"
		embKey    = "emb-live-0123456789abcdef"
		s3Access  = "AKIAIOSFODNN7EXAMPLE"
		s3Secret  = "s3-secret-0123456789abcdef"
		skillKey  = "skill-live-0123456789abcdef"
		envSecret = "env-secret-0123456789abcdef"
	)
	secrets := []string{provKey, e2bKey, boxKey, hooksTok, botTok, acctTok, qqSecret, embKey, s3Access, s3Secret, skillKey, envSecret}

	// Seed one provider row (the configs-table shape GET /api/providers reads).
	if err := scope.SaveProviderByScope(ctx, s.dataStore, scope.System, "", "openai", config.ProviderConfig{
		APIKey:  provKey,
		APIBase: "https://api.openai.com/v1",
	}); err != nil {
		t.Fatalf("SaveProviderByScope: %v", err)
	}
	// Seed the namespaces GET /api/config assembles.
	nsSeeds := []struct {
		ns   string
		data map[string]interface{}
	}{
		{"sandbox", map[string]interface{}{"enabled": true, "e2bKey": e2bKey, "boxliteKey": boxKey}},
		{"hooks", map[string]interface{}{"enabled": true, "token": hooksTok}},
		{"memory", map[string]interface{}{"embedding": map[string]interface{}{"enabled": true, "apiKey": embKey}}},
		{"objectstore", map[string]interface{}{"s3": map[string]interface{}{"accessKey": s3Access, "secretKey": s3Secret, "bucket": "fluctio"}}},
		{"skills.entries", map[string]interface{}{"my-skill": map[string]interface{}{"enabled": true, "apiKey": skillKey, "env": map[string]interface{}{"SKILL_TOKEN": envSecret}}}},
	}
	for _, seed := range nsSeeds {
		if err := scope.SaveSettingByScope(ctx, s.dataStore, scope.System, "", seed.ns, seed.data); err != nil {
			t.Fatalf("SaveSettingByScope(%s): %v", seed.ns, err)
		}
	}
	if err := scope.SaveChannelByScope(ctx, s.dataStore, scope.System, "", "telegram", acctTok, true, config.ChannelConfig{
		Enabled:  true,
		BotToken: botTok,
		Accounts: map[string]config.AccountConfig{
			"qq-main": {BotToken: acctTok, ClientSecret: qqSecret},
		},
	}); err != nil {
		t.Fatalf("SaveChannelByScope: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"GET /api/config", "/api/config", s.handleGetConfig},
		{"GET /api/providers", "/api/providers", s.handleListProviders},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := authTestRequest(t, ctx, resolver, http.MethodGet, tc.path, adminUser.ID)
			rr := httptest.NewRecorder()
			s.authMiddleware(tc.handler)(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			for _, secret := range secrets {
				if strings.Contains(body, secret) {
					t.Errorf("%s: response leaks plaintext secret %q", tc.name, secret)
				}
			}
		})
	}

	// Round-trip guard: echoing the masked GET response back through POST
	// /api/config must restore the stored secrets from the masked
	// sentinel, not persist the masks over them.
	t.Run("POST /api/config keeps stored secrets on masked echo", func(t *testing.T) {
		maskedE2B := config.MaskSecret(e2bKey) // what the settings page received
		maskedBox := config.MaskSecret(boxKey)
		body := `{"sandbox":{"enabled":true,"e2bKey":"` + maskedE2B + `","boxliteKey":"` + maskedBox + `"}}`
		post := authTestRequest(t, ctx, resolver, http.MethodPost, "/api/config", adminUser.ID)
		setRequestBody(t, post, body)

		rr := httptest.NewRecorder()
		s.authMiddleware(s.handleUpdateConfig)(rr, post)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var stored map[string]interface{}
		if err := scope.SettingInto(ctx, s.dataStore, "sandbox", "", "", &stored); err != nil {
			t.Fatalf("read back sandbox: %v", err)
		}
		if got, _ := stored["e2bKey"].(string); got != e2bKey {
			t.Errorf("stored e2bKey = %q, want the original plaintext (mask must not be persisted)", got)
		}
		if got, _ := stored["boxliteKey"].(string); got != boxKey {
			t.Errorf("stored boxliteKey = %q, want the original plaintext", got)
		}
	})
}

func setRequestBody(t *testing.T, r *http.Request, body string) {
	t.Helper()
	r.Body = io.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
}
