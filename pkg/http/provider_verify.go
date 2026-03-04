package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/epitchi/goclaw/pkg/providers"
)

// handleVerifyProvider tests a provider+model combination with a minimal LLM call.
//
//	POST /v1/providers/{id}/verify
//	Body: {"model": "anthropic/claude-sonnet-4"}
//	Response: {"valid": true} or {"valid": false, "error": "..."}
func (h *ProvidersHandler) handleVerifyProvider(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid provider ID"})
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}

	// Look up provider record from DB to get the provider name
	p, err := h.store.GetProvider(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found"})
		return
	}

	if h.providerReg == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": "no provider registry available"})
		return
	}

	provider, err := h.providerReg.Get(p.Name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": "provider not registered: " + p.Name})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	_, err = provider.Chat(ctx, providers.ChatRequest{
		Messages: []providers.Message{
			{Role: "user", Content: "hi"},
		},
		Model: req.Model,
		Options: map[string]interface{}{
			"max_tokens": 1,
		},
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"valid": false, "error": friendlyVerifyError(err)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"valid": true})
}

// friendlyVerifyError extracts a human-readable message from provider errors.
// Raw errors often contain JSON blobs like: `HTTP 400: minimax: {"type":"error","error":{"type":"bad_request_error","message":"unknown model ..."}}`
func friendlyVerifyError(err error) string {
	msg := err.Error()

	// Try to extract "message" field from embedded JSON
	if idx := strings.Index(msg, `"message"`); idx >= 0 {
		// Find the value after "message":
		rest := msg[idx:]
		// Look for :"<value>"
		start := strings.Index(rest, `:`)
		if start >= 0 {
			rest = strings.TrimLeft(rest[start+1:], " ")
			if len(rest) > 0 && rest[0] == '"' {
				rest = rest[1:]
				if end := strings.Index(rest, `"`); end >= 0 {
					extracted := rest[:end]
					if extracted != "" {
						return extracted
					}
				}
			}
		}
	}

	// Fallback: strip "HTTP NNN: provider: " prefix for cleaner display
	if idx := strings.LastIndex(msg, ": "); idx >= 0 && idx < len(msg)-2 {
		suffix := msg[idx+2:]
		// If the remainder still looks like JSON, just say "invalid model"
		if strings.HasPrefix(suffix, "{") {
			return "Model not recognized by provider"
		}
		return suffix
	}

	return msg
}
