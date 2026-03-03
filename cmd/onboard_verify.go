package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/pkg/config"
	"github.com/nextlevelbuilder/goclaw/pkg/providers"
)

// providerVerifyError holds the result of a provider connectivity probe.
type providerVerifyError struct {
	fatal   bool   // true = bad credentials, block startup
	message string // human-readable description
}

func (e *providerVerifyError) Error() string { return e.message }

// newProviderForVerify instantiates a temporary provider for key verification.
// Mirrors the logic in registerProviders (gateway_providers.go) so auth headers,
// base URL overrides, and custom chat paths are handled correctly.
func newProviderForVerify(cfg *config.Config, name string) providers.Provider {
	apiKey := resolveProviderAPIKey(cfg, name)
	apiBase := resolveProviderAPIBase(name)

	switch name {
	case "anthropic":
		if cfg.Providers.Anthropic.APIBase != "" {
			apiBase = cfg.Providers.Anthropic.APIBase
		}
		return providers.NewAnthropicProvider(apiKey, providers.WithAnthropicBaseURL(apiBase))
	case "dashscope":
		if cfg.Providers.DashScope.APIBase != "" {
			apiBase = cfg.Providers.DashScope.APIBase
		}
		return providers.NewDashScopeProvider(apiKey, apiBase, "")
	case "minimax":
		return providers.NewOpenAIProvider(name, apiKey, apiBase, "").WithChatPath("/text/chatcompletion_v2")
	case "openai":
		if cfg.Providers.OpenAI.APIBase != "" {
			apiBase = cfg.Providers.OpenAI.APIBase
		}
		return providers.NewOpenAIProvider(name, apiKey, apiBase, "")
	case "bailian":
		if cfg.Providers.Bailian.APIBase != "" {
			apiBase = cfg.Providers.Bailian.APIBase
		}
		return providers.NewOpenAIProvider(name, apiKey, apiBase, "")
	default:
		return providers.NewOpenAIProvider(name, apiKey, apiBase, "")
	}
}

// verifyProviderConnectivity checks whether a provider's API key is valid by
// sending a minimal Chat request through the provider layer. This reuses the
// same auth headers, base URLs, and HTTP client as the real provider calls.
//
// Strategy: provider.Chat() with message "hi", max_tokens=1.
//   - 401/403 HTTPError → invalid API key (fatal)
//   - Any other error   → non-fatal warning (transient/config issue)
//   - Success           → key is valid
func verifyProviderConnectivity(cfg *config.Config, providerName string) *providerVerifyError {
	apiKey := resolveProviderAPIKey(cfg, providerName)
	if apiKey == "" {
		return nil
	}

	apiBase := resolveProviderAPIBase(providerName)
	if apiBase == "" {
		return nil // custom/unknown provider — skip
	}

	prov := newProviderForVerify(cfg, providerName)

	model := ""
	if pi, ok := providerMap[providerName]; ok {
		model = pi.modelHint
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := prov.Chat(ctx, providers.ChatRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Model:    model,
		Options:  map[string]interface{}{"max_tokens": 1},
	})
	if err != nil {
		var httpErr *providers.HTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			return &providerVerifyError{
				fatal:   true,
				message: fmt.Sprintf("%s returned %d — invalid API key", providerName, httpErr.Status),
			}
		}
		// Non-auth errors: transient network issue, bad model, rate limit, etc.
		return &providerVerifyError{
			fatal:   false,
			message: fmt.Sprintf("%s: %s", providerName, friendlyProviderError(err)),
		}
	}

	return nil
}

// verifyAllProviders checks connectivity for every provider that has an API key.
// Only the primary provider's auth failure is fatal (blocks bootstrap).
// Secondary provider failures are logged as warnings.
func verifyAllProviders(cfg *config.Config, primaryProvider string) []string {
	var fatalErrors []string

	for _, name := range providerPriority {
		apiKey := resolveProviderAPIKey(cfg, name)
		if apiKey == "" {
			continue
		}

		verr := verifyProviderConnectivity(cfg, name)
		if verr == nil {
			slog.Info("provider connectivity verified", "provider", name)
			fmt.Printf("    %s: OK\n", name)
			continue
		}

		if verr.fatal && name == primaryProvider {
			slog.Error("primary provider key invalid", "provider", name, "error", verr.message)
			fmt.Printf("    %s: FAILED — %s\n", name, verr.message)
			fatalErrors = append(fatalErrors, fmt.Sprintf("%s: %s", name, verr.message))
		} else if verr.fatal {
			slog.Warn("secondary provider key invalid (continuing)", "provider", name, "error", verr.message)
			fmt.Printf("    %s: WARNING — %s (non-primary, skipping)\n", name, verr.message)
		} else {
			slog.Warn("provider connectivity warning", "provider", name, "warning", verr.message)
			fmt.Printf("    %s: WARNING — %s\n", name, verr.message)
		}
	}

	return fatalErrors
}

// friendlyProviderError extracts a human-readable message from provider errors.
func friendlyProviderError(err error) string {
	msg := err.Error()

	// Try to extract "message" field from embedded JSON error blobs.
	if idx := strings.Index(msg, `"message"`); idx >= 0 {
		rest := msg[idx:]
		if start := strings.Index(rest, `:`); start >= 0 {
			rest = strings.TrimLeft(rest[start+1:], " ")
			if len(rest) > 0 && rest[0] == '"' {
				rest = rest[1:]
				if end := strings.Index(rest, `"`); end >= 0 && rest[:end] != "" {
					return rest[:end]
				}
			}
		}
	}

	// Strip "HTTP NNN: provider: " prefix.
	if idx := strings.LastIndex(msg, ": "); idx >= 0 && idx < len(msg)-2 {
		suffix := msg[idx+2:]
		if strings.HasPrefix(suffix, "{") {
			return "request rejected by provider"
		}
		return suffix
	}

	return msg
}
