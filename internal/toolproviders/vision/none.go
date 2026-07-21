package vision

import (
	"context"

	"github.com/fluctio-ai/fluctio/internal/toolproviders"
)

// None is a sentinel provider meaning "do not expose vision to the model."
// The tool-registration layer detects "none" anywhere in the chain and
// skips registering the tool entirely, so the model either uses its own
// native multimodal capability (if it has one) or does without.
//
// CredentialFree lets chain.Available() report true when "none" is the
// only configured provider, so the dashboard can distinguish an explicit
// choice from "forgot to configure".
type None struct{}

func (None) Category() string     { return Category }
func (None) Name() string         { return "none" }
func (None) CredentialFree() bool { return true }

// Execute should never be reached: registration short-circuits on "none".
func (None) Execute(_ context.Context, _ toolproviders.Request) (toolproviders.Response, error) {
	return toolproviders.Response{}, toolproviders.ErrNoResults
}
