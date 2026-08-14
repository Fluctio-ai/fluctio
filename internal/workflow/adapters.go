package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fluctio-ai/fluctio/internal/provider"
	"github.com/fluctio-ai/fluctio/internal/sandbox"
)

// ProviderLLMCaller adapts a provider.Provider to LLMCaller: it issues a single
// bare (no-tools) user-message call and returns the model's raw content, which
// the runner then parses against the node's output schema. Model / token /
// temperature are configured per caller — the tracer bullet carries no
// per-node model field; that arrives with Seam B (ticket 02).
type ProviderLLMCaller struct {
	P         provider.Provider
	Model     string
	MaxTokens int
	Temp      float64
}

// Call implements LLMCaller.
func (c *ProviderLLMCaller) Call(ctx context.Context, prompt string) (string, error) {
	resp, err := c.P.Chat(ctx,
		[]provider.Message{{Role: "user", Content: prompt}},
		nil, c.Model, c.MaxTokens, c.Temp)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// ToolExecutor is the slice of *tools.Registry the runner's tool caller needs.
// Declared locally so this package does not import internal/agent/tools (which
// is agent-scoped and heavyweight); *tools.Registry satisfies it structurally.
type ToolExecutor interface {
	Execute(ctx context.Context, name, args string) (string, error)
}

// RegistryToolCaller adapts a tool registry to ToolCaller: it serialises the
// resolved args map to JSON and delegates to Execute. The raw string the
// registry returns is parsed by the runner's parseOutput.
type RegistryToolCaller struct {
	R ToolExecutor
}

// Call implements ToolCaller.
func (c *RegistryToolCaller) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal tool args: %w", err)
	}
	return c.R.Execute(ctx, name, string(b))
}

// codeExecTimeout caps one code-node script run — the workflow-level ceiling
// over the sandbox backend's own timeout.
const codeExecTimeout = 30 * time.Second

// langSpec maps a node Language to its file extension + interpreter command.
// Validate rejects unknown languages; this map is the runtime fallback for the
// default ("python") and the accepted aliases.
var langSpec = map[string]struct{ ext, interpreter string }{
	"python": {".py", "python"},
	"py":     {".py", "python"},
	"sh":     {".sh", "sh"},
	"shell":  {".sh", "sh"},
}

// SandboxCodeCaller adapts a sandbox.Executor to CodeCaller (spec decision 3):
// it writes the script to a uniquely-named temp file inside the sandbox and
// runs it with the language interpreter, returning combined stdout+stderr. The
// runner parses stdout against the node's output schema, so a well-behaved
// script prints only its JSON result to stdout (stderr stays free for logs). A
// non-zero exit comes back as an error carrying the script's combined output —
// the actionable diagnostic (e.g. a python traceback).
type SandboxCodeCaller struct {
	Ex sandbox.Executor
}

// Run implements CodeCaller.
func (c *SandboxCodeCaller) Run(ctx context.Context, language, code string) (string, error) {
	spec, ok := langSpec[language]
	if !ok {
		return "", fmt.Errorf("unsupported language %q", language)
	}
	name := "wf_code_" + randSuffix(8) + spec.ext
	if _, err := c.Ex.WriteFile(ctx, name, code); err != nil {
		return "", fmt.Errorf("write script: %w", err)
	}
	out, err := c.Ex.Exec(ctx, spec.interpreter+" "+name, codeExecTimeout)
	if err != nil {
		if out != "" {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
		}
		return "", err
	}
	return out, nil
}

// httpExecTimeout caps one http-node request — the workflow-level ceiling,
// matching the code-node limit.
const httpExecTimeout = 30 * time.Second

// NetHTTPCaller is the production HTTPCaller for the M3 http node: a stateless
// net/http client. It builds the request from the node's resolved input,
// forwards declared headers, and returns the final status + raw body (the
// runner parses the body into {status, body}).
type NetHTTPCaller struct{}

// Do implements HTTPCaller.
func (NetHTTPCaller) Do(ctx context.Context, method, url string, headers map[string]string, body string) (int, string, error) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: httpExecTimeout}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, string(b), nil
}

func randSuffix(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewProviderRunner is the production wiring: a provider-backed LLM caller,
// a registry-backed tool caller over the given persistence seam. Callers pick
// model / maxTokens / temperature; per-node model selection comes later.
func NewProviderRunner(p provider.Provider, model string, maxTokens int, temp float64, reg ToolExecutor, code CodeCaller, rs RunStore) *Runner {
	return NewRunner(
		&ProviderLLMCaller{P: p, Model: model, MaxTokens: maxTokens, Temp: temp},
		&RegistryToolCaller{R: reg},
		rs,
		WithCodeCaller(code),
	)
}
