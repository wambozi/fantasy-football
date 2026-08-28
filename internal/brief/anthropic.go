package brief

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is what spec §7 asks for. Override with BRIEF_MODEL.
const DefaultModel = "claude-sonnet-5"

// KeyFromEnv returns the API key: FANTASY_ANTHROPIC_API_KEY first (this project's
// dedicated key), then ANTHROPIC_API_KEY. Empty when neither is set.
func KeyFromEnv() string {
	for _, k := range []string{"FANTASY_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// Anthropic is the Messages API generator.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic builds a generator. model "" uses BRIEF_MODEL or DefaultModel.
func NewAnthropic(apiKey, model string) *Anthropic {
	if model == "" {
		model = os.Getenv("BRIEF_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{client: anthropic.NewClient(option.WithAPIKey(apiKey), option.WithMaxRetries(1)), model: model}
}

// Model reports the configured model id.
func (a *Anthropic) Model() string { return a.model }

// Generate makes one Messages call. The system prompt is marked for prompt caching:
// it is identical for the whole draft, only the user JSON varies (spec §7).
func (a *Anthropic) Generate(ctx context.Context, system, user string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 400,
		System: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		return "", err
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", errors.New("model refused")
	}
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("empty response (stop=%s)", resp.StopReason)
	}
	return b.String(), nil
}
