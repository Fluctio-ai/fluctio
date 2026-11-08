package users

import (
	"context"
	"strings"
	"testing"

	"github.com/fluctio-ai/fluctio/internal/store"
)

// keyType lands in the auth path — arbitrary strings must be refused
// instead of minting a token whose permission semantics are undefined.
func TestAPIKeysCreateRejectsUnknownKeyType(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ak, err := NewAPIKeys(db)
	if err != nil {
		t.Fatalf("NewAPIKeys: %v", err)
	}
	_, _, err = ak.Create(context.Background(), "u_1", "n", "superuser", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown key type") {
		t.Fatalf("unknown keyType accepted: %v", err)
	}

	// The only legal value keeps working.
	rec, _, err := ak.Create(context.Background(), "u_1", "n", APIKeyTypeAdmin, nil)
	if err != nil {
		t.Fatalf("admin key create: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("empty key id")
	}
}
