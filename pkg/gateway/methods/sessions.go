package methods

import (
	"context"
	"encoding/json"

	"github.com/epitchi/goclaw/pkg/gateway"
	"github.com/epitchi/goclaw/pkg/store"
	"github.com/epitchi/goclaw/pkg/protocol"
)

// SessionsMethods handles sessions.list, sessions.preview, sessions.patch, sessions.delete, sessions.reset.
type SessionsMethods struct {
	sessions store.SessionStore
}

func NewSessionsMethods(sess store.SessionStore) *SessionsMethods {
	return &SessionsMethods{sessions: sess}
}

func (m *SessionsMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodSessionsList, m.handleList)
	router.Register(protocol.MethodSessionsPreview, m.handlePreview)
	router.Register(protocol.MethodSessionsPatch, m.handlePatch)
	router.Register(protocol.MethodSessionsDelete, m.handleDelete)
	router.Register(protocol.MethodSessionsReset, m.handleReset)
}

type sessionsListParams struct {
	AgentID string `json:"agentId"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
}

func (m *SessionsMethods) handleList(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params sessionsListParams
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}

	if params.Limit <= 0 {
		params.Limit = 20
	}

	result := m.sessions.ListPaged(store.SessionListOpts{
		AgentID: params.AgentID,
		Limit:   params.Limit,
		Offset:  params.Offset,
	})
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"sessions": result.Sessions,
		"total":    result.Total,
		"limit":    params.Limit,
		"offset":   params.Offset,
	}))
}

type sessionKeyParams struct {
	Key string `json:"key"`
}

func (m *SessionsMethods) handlePreview(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params sessionKeyParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	history := m.sessions.GetHistory(params.Key)
	summary := m.sessions.GetSummary(params.Key)

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"key":      params.Key,
		"messages": history,
		"summary":  summary,
	}))
}

// handlePatch updates session metadata fields.
// Matching TS sessions.patch (src/gateway/server-methods/sessions.ts:237-287).
func (m *SessionsMethods) handlePatch(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params struct {
		Key   string `json:"key"`
		Label *string `json:"label,omitempty"`
		Model *string `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.Key == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "key is required"))
		return
	}

	// Apply label patch
	if params.Label != nil {
		m.sessions.SetLabel(params.Key, *params.Label)
	}

	// Apply model patch
	if params.Model != nil {
		m.sessions.UpdateMetadata(params.Key, *params.Model, "", "")
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"ok":  true,
		"key": params.Key,
	}))
}

func (m *SessionsMethods) handleDelete(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params sessionKeyParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if err := m.sessions.Delete(params.Key); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"ok": true,
	}))
}

func (m *SessionsMethods) handleReset(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	var params sessionKeyParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	m.sessions.Reset(params.Key)

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"ok": true,
	}))
}
