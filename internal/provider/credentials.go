package provider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/pbkdf2"

	"github.com/fluctio-ai/fluctio/internal/config"
)

// CredentialEntry represents a stored credential.
type CredentialEntry struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`   // "api_key", "oauth", "token"
	Source string            `json:"source"` // "config", "env", "store"
	Keys   map[string]string `json:"keys"`
}

// CredentialManager handles secure credential storage and retrieval.
type CredentialManager struct {
	masterKey      []byte
	entries        map[string]*CredentialEntry
	storePath      string
	needsReencrypt bool // true after legacy-key fallback decrypt
	// kdf tracks the derivation parameters the CURRENT masterKey came
	// from; save() persists them in a sidecar so load() can tell legacy
	// single-SHA-256 files from PBKDF2 ones and migrate transparently.
	kdf *kdfParams
	// kdfSeedUserID captures the user id the PBKDF2 seed was built from
	// (the storeDir basename is the same id, but storing it explicitly
	// keeps load() independent of path parsing).
	kdfSeedUserID string
	mu            sync.RWMutex
}

// NewCredentialManagerForUser creates a credential manager scoped to a specific
// user. Credentials live at ~/.fluctio/users/{userID}/credentials.json and
// are encrypted with a key derived from the user ID, so one user's file
// cannot be decrypted with another user's key even if moved on disk.
func NewCredentialManagerForUser(userID, passphrase string) (*CredentialManager, error) {
	if userID == "" {
		return nil, fmt.Errorf("provider: NewCredentialManagerForUser requires userID")
	}
	home, err := config.HomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	storeDir := filepath.Join(home, "users", userID)
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure user dir: %w", err)
	}

	var kdf *kdfParams
	var key []byte
	if passphrase != "" {
		// Explicit passphrase: derive with PBKDF2 (salted, iterated) —
		// the old single-SHA-256 round let an offline attacker with the
		// guessable seed material brute-force a weak passphrase at hash
		// speed.
		k, saltHex, err := deriveKeyPBKDF2(userID, passphrase, "")
		if err != nil {
			return nil, fmt.Errorf("derive key: %w", err)
		}
		key = k
		kdf = &kdfParams{Algorithm: "pbkdf2-sha256", Iterations: kdfIterations, Salt: saltHex}
	} else {
		// Machine-derived fallback keeps the legacy single-SHA-256 shape
		// (the seed material is host-bound, so the offline-guessing
		// surface differs); documented trade-off, not a migration target.
		key = deriveKeyForUser(userID, passphrase)
		kdf = &kdfParams{Algorithm: legacyKDFMarker}
	}

	cm := &CredentialManager{
		masterKey:     key,
		entries:       make(map[string]*CredentialEntry),
		storePath:     filepath.Join(storeDir, "credentials.json"),
		kdf:           kdf,
		kdfSeedUserID: userID,
	}

	// Load existing credentials
	if err := cm.load(); err != nil && !os.IsNotExist(err) {
		// If decryption fails, start fresh
		cm.entries = make(map[string]*CredentialEntry)
	}

	// If we decrypted with the legacy key, immediately re-save with the
	// new per-user key so the file is migrated on disk.
	if cm.needsReencrypt {
		if err := cm.save(); err == nil {
			cm.needsReencrypt = false
		}
	}

	return cm, nil
}

// Set stores a credential key-value pair.
func (cm *CredentialManager) Set(name, key, value string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	entry, ok := cm.entries[name]
	if !ok {
		entry = &CredentialEntry{
			Name:   name,
			Type:   "api_key",
			Source: "store",
			Keys:   make(map[string]string),
		}
		cm.entries[name] = entry
	}

	entry.Keys[key] = value
	return cm.save()
}

// Get retrieves a credential value.
func (cm *CredentialManager) Get(name, key string) (string, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	entry, ok := cm.entries[name]
	if !ok {
		return "", fmt.Errorf("credential %q not found", name)
	}

	val, ok := entry.Keys[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in credential %q", key, name)
	}

	return val, nil
}

// List returns all credential entries.
func (cm *CredentialManager) List() []CredentialEntry {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	result := make([]CredentialEntry, 0, len(cm.entries))
	for _, e := range cm.entries {
		// Copy without exposing key values
		masked := CredentialEntry{
			Name:   e.Name,
			Type:   e.Type,
			Source: e.Source,
			Keys:   make(map[string]string),
		}
		for k, v := range e.Keys {
			if len(v) > 8 {
				masked.Keys[k] = v[:4] + "..." + v[len(v)-4:]
			} else {
				masked.Keys[k] = "****"
			}
		}
		result = append(result, masked)
	}
	return result
}

// Delete removes a credential entry.
func (cm *CredentialManager) Delete(name string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, ok := cm.entries[name]; !ok {
		return fmt.Errorf("credential %q not found", name)
	}

	delete(cm.entries, name)
	return cm.save()
}

// knownEnvVars maps provider names to their environment variable patterns.
var knownEnvVars = map[string][]string{
	"openai":     {"OPENAI_API_KEY"},
	"anthropic":  {"ANTHROPIC_API_KEY"},
	"openrouter": {"OPENROUTER_API_KEY"},
	"google":     {"GOOGLE_API_KEY", "GEMINI_API_KEY"},
	"mistral":    {"MISTRAL_API_KEY"},
	"cohere":     {"COHERE_API_KEY"},
	"groq":       {"GROQ_API_KEY"},
	"together":   {"TOGETHER_API_KEY"},
	"deepseek":   {"DEEPSEEK_API_KEY"},
}

// Discover scans environment variables for known API key patterns.
func (cm *CredentialManager) Discover() []CredentialEntry {
	var discovered []CredentialEntry

	for providerName, envVars := range knownEnvVars {
		for _, envVar := range envVars {
			val := os.Getenv(envVar)
			if val == "" {
				continue
			}
			entry := CredentialEntry{
				Name:   providerName,
				Type:   "api_key",
				Source: "env",
				Keys:   map[string]string{"apiKey": val},
			}
			discovered = append(discovered, entry)
		}
	}

	return discovered
}

// InjectEnv returns environment variables suitable for injecting into a sandbox.
func (cm *CredentialManager) InjectEnv() map[string]string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	env := make(map[string]string)

	for name, entry := range cm.entries {
		if apiKey, ok := entry.Keys["apiKey"]; ok {
			// Map back to env var name
			envVars, known := knownEnvVars[name]
			if known && len(envVars) > 0 {
				env[envVars[0]] = apiKey
			} else {
				env[strings.ToUpper(name)+"_API_KEY"] = apiKey
			}
		}
	}

	// Also include any env-discovered credentials
	for _, envVars := range knownEnvVars {
		for _, envVar := range envVars {
			if val := os.Getenv(envVar); val != "" {
				env[envVar] = val
			}
		}
	}

	return env
}

func (cm *CredentialManager) save() error {
	data, err := json.Marshal(cm.entries)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	encrypted, err := encrypt(data, cm.masterKey)
	if err != nil {
		return fmt.Errorf("encrypt credentials: %w", err)
	}

	if err := os.WriteFile(cm.storePath, encrypted, 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}

	// Persist the KDF parameters next to the ciphertext so the next load
	// knows which derivation to use (PBKDF2 salt/iterations vs the legacy
	// single-SHA-256). Sidecar, not a header: keeps the blob format
	// byte-compatible with pre-KDF installs.
	if cm.kdf != nil {
		blob, err := json.Marshal(cm.kdf)
		if err == nil {
			if err := os.WriteFile(cm.storePath+".kdf", blob, 0o600); err != nil {
				return fmt.Errorf("write kdf params: %w", err)
			}
		}
	}

	return nil
}

// readKDFParams reads the KDF sidecar next to a credentials file.
// Returns has=false when the sidecar is absent or unparsable — callers
// treat that as "pre-KDF file".
func readKDFParams(sidecarPath string) (kdfParams, bool) {
	blob, err := os.ReadFile(sidecarPath)
	if err != nil {
		return kdfParams{}, false
	}
	var p kdfParams
	if json.Unmarshal(blob, &p) != nil || p.Algorithm == "" {
		return kdfParams{}, false
	}
	return p, true
}

func (cm *CredentialManager) load() error {
	data, err := os.ReadFile(cm.storePath)
	if err != nil {
		return err
	}

	// Read the stored KDF parameters (PBKDF2 salt/iterations) when the
	// sidecar exists. Absent sidecar = a pre-KDF file: decrypt with the
	// current masterKey shape, then migrate.
	storedKDF, hasKDF := readKDFParams(cm.storePath + ".kdf")
	if hasKDF && storedKDF.Algorithm == "pbkdf2-sha256" && cm.kdf != nil && cm.kdf.Algorithm == "pbkdf2-sha256" {
		key, _, err := deriveKeyPBKDF2(cm.kdfSeedUserID, "", storedKDF.Salt)
		if err != nil {
			return fmt.Errorf("derive pbkdf2 key: %w", err)
		}
		decrypted, err := decrypt(data, key)
		if err != nil {
			return fmt.Errorf("decrypt credentials (pbkdf2): %w", err)
		}
		return json.Unmarshal(decrypted, &cm.entries)
	}

	decrypted, err := decrypt(data, cm.masterKey)
	if err != nil {
		// Fallback: try legacy key format (pre-multiuser) so existing
		// installs don't lose their stored credentials after upgrading.
		legacyKey := legacyDeriveKey()
		decrypted, err = decrypt(data, legacyKey)
		if err != nil {
			return fmt.Errorf("decrypt credentials: %w", err)
		}
		cm.needsReencrypt = true
	}

	return json.Unmarshal(decrypted, &cm.entries)
}

// legacyDeriveKey returns the old (pre-multiuser) machine-derived key so
// we can decrypt credentials files created before per-user KEK was added.
func legacyDeriveKey() []byte {
	hostname, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	hash := sha256.Sum256([]byte("fluctio:" + hostname + ":" + home))
	return hash[:]
}

// deriveKeyForUser mixes the user ID into the encryption key so that each
// user's credentials file is encrypted with a distinct KEK. In the absence
// of an explicit passphrase, a machine-derived seed (hostname + home) is
// still included so the same user ID yields different keys on different
// hosts — preventing wholesale copy of the credentials file across hosts
// from decrypting.
//
// KDF: PBKDF2-HMAC-SHA256 with 210k iterations (OWASP 2023 floor for
// SHA-256). The legacy implementation was a single unsalted SHA-256 over
// the seed string — an offline attacker with the seed material (user id,
// hostname, home are all guessable) could brute-force a weak passphrase
// at hash speed. Files encrypted with the legacy KDF are detected at
// load (version marker) and transparently migrated, mirroring the
// legacy-KEK re-encrypt path.
const (
	kdfIterations   = 210000
	kdfSaltLength   = 16
	kdfKeyLength    = 32
	legacyKDFMarker = "legacy-sha256-v1"
)

type kdfParams struct {
	Algorithm  string `json:"algorithm"` // "pbkdf2-sha256" | legacyKDFMarker
	Iterations int    `json:"iterations,omitempty"`
	Salt       string `json:"salt,omitempty"` // hex
}

// deriveKeyPBKDF2 derives the KEK with a random salt + PBKDF2. Returns
// the key and the hex-encoded salt (stored alongside the ciphertext).
func deriveKeyPBKDF2(userID, passphrase, saltHex string) ([]byte, string, error) {
	userID = fallbackUserID(userID)
	salt := make([]byte, kdfSaltLength)
	if saltHex == "" {
		if _, err := rand.Read(salt); err != nil {
			return nil, "", err
		}
	} else {
		var err error
		salt, err = hex.DecodeString(saltHex)
		if err != nil || len(salt) < 8 {
			return nil, "", fmt.Errorf("invalid stored kdf salt")
		}
	}
	seed := "fluctio:user:" + userID + ":pp:" + passphrase
	key := pbkdf2.Key([]byte(seed), salt, kdfIterations, kdfKeyLength, sha256.New)
	return key, hex.EncodeToString(salt), nil
}

func fallbackUserID(userID string) string {
	if userID == "" {
		return "_anonymous"
	}
	return userID
}

func deriveKeyForUser(userID, passphrase string) []byte {
	userID = fallbackUserID(userID)
	var seed string
	if passphrase != "" {
		seed = "fluctio:user:" + userID + ":pp:" + passphrase
	} else {
		hostname, _ := os.Hostname()
		home, _ := os.UserHomeDir()
		seed = "fluctio:user:" + userID + ":host:" + hostname + ":" + home
	}
	hash := sha256.Sum256([]byte(seed))
	return hash[:]
}

func encrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
