package gateway

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/epitchi/goclaw/pkg/permissions"
	"github.com/epitchi/goclaw/pkg/protocol"
)

// MethodHandler processes a single RPC method request.
type MethodHandler func(ctx context.Context, client *Client, req *protocol.RequestFrame)

// MethodRouter maps method names to handlers.
type MethodRouter struct {
	handlers map[string]MethodHandler
	server   *Server
}

func NewMethodRouter(server *Server) *MethodRouter {
	r := &MethodRouter{
		handlers: make(map[string]MethodHandler),
		server:   server,
	}
	r.registerDefaults()
	return r
}

// Register adds a method handler.
func (r *MethodRouter) Register(method string, handler MethodHandler) {
	r.handlers[method] = handler
}

// Handle dispatches a request to the appropriate handler.
func (r *MethodRouter) Handle(ctx context.Context, client *Client, req *protocol.RequestFrame) {
	handler, ok := r.handlers[req.Method]
	if !ok {
		slog.Warn("unknown method", "method", req.Method, "client", client.id)
		client.SendResponse(protocol.NewErrorResponse(
			req.ID,
			protocol.ErrInvalidRequest,
			"unknown method: "+req.Method,
		))
		return
	}

	// Permission check: skip for connect, health, and browser pairing status (used by unauthenticated clients)
	if req.Method != protocol.MethodConnect && req.Method != protocol.MethodHealth && req.Method != protocol.MethodBrowserPairingStatus {
		if pe := r.server.policyEngine; pe != nil {
			if !pe.CanAccess(client.role, req.Method) {
				slog.Warn("permission denied", "method", req.Method, "role", client.role, "client", client.id)
				client.SendResponse(protocol.NewErrorResponse(
					req.ID,
					protocol.ErrUnauthorized,
					"permission denied: insufficient role for "+req.Method,
				))
				return
			}
		}
	}

	slog.Debug("handling method", "method", req.Method, "client", client.id, "req_id", req.ID)
	handler(ctx, client, req)
}

// registerDefaults registers built-in Phase 1 method handlers.
func (r *MethodRouter) registerDefaults() {
	// System
	r.Register(protocol.MethodConnect, r.handleConnect)
	r.Register(protocol.MethodHealth, r.handleHealth)
	r.Register(protocol.MethodStatus, r.handleStatus)
}

// --- Built-in handlers ---

func (r *MethodRouter) handleConnect(ctx context.Context, client *Client, req *protocol.RequestFrame) {
	// Parse connect params
	var params struct {
		Token    string `json:"token"`
		UserID   string `json:"user_id"`
		SenderID string `json:"sender_id"` // browser pairing: stored sender ID for reconnect
	}
	if req.Params != nil {
		json.Unmarshal(req.Params, &params)
	}

	configToken := r.server.cfg.Gateway.Token

	// Path 1: Valid token → admin
	if configToken != "" && params.Token == configToken {
		client.role = permissions.RoleAdmin
		client.authenticated = true
		client.userID = params.UserID
		r.sendConnectResponse(client, req.ID)
		return
	}

	// Path 2: No token configured → operator (backward compat)
	if configToken == "" {
		client.role = permissions.RoleOperator
		client.authenticated = true
		client.userID = params.UserID
		r.sendConnectResponse(client, req.ID)
		return
	}

	// Path 3: Token configured but not provided/wrong → check browser pairing
	ps := r.server.pairingService

	// Path 3a: Reconnecting with a previously-paired sender_id
	if ps != nil && params.SenderID != "" && ps.IsPaired(params.SenderID, "browser") {
		client.role = permissions.RoleOperator
		client.authenticated = true
		client.userID = params.UserID
		slog.Info("browser pairing authenticated", "sender_id", params.SenderID, "client", client.id)
		r.sendConnectResponse(client, req.ID)
		return
	}

	// Path 3b: No token, no valid pairing → initiate browser pairing (if service available)
	if ps != nil && params.Token == "" {
		code, err := ps.RequestPairing(client.id, "browser", "", "default")
		if err != nil {
			slog.Warn("browser pairing request failed", "error", err, "client", client.id)
			// Fall through to viewer role
		} else {
			client.pairingCode = code
			client.pairingPending = true
			// Not authenticated — can only call browser.pairing.status
			client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
				"protocol":     protocol.ProtocolVersion,
				"status":       "pending_pairing",
				"pairing_code": code,
				"sender_id":    client.id,
				"server": map[string]interface{}{
					"name":    "goclaw",
					"version": "0.2.0",
				},
			}))
			return
		}
	}

	// Path 4: Fallback → viewer (wrong token or pairing not available)
	client.role = permissions.RoleViewer
	client.authenticated = true
	client.userID = params.UserID
	r.sendConnectResponse(client, req.ID)
}

func (r *MethodRouter) sendConnectResponse(client *Client, reqID string) {
	client.SendResponse(protocol.NewOKResponse(reqID, map[string]interface{}{
		"protocol": protocol.ProtocolVersion,
		"role":     string(client.role),
		"user_id":  client.userID,
		"server": map[string]interface{}{
			"name":    "goclaw",
			"version": "0.2.0",
		},
	}))
}

func (r *MethodRouter) handleHealth(ctx context.Context, client *Client, req *protocol.RequestFrame) {
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"status": "ok",
	}))
}

func (r *MethodRouter) handleStatus(ctx context.Context, client *Client, req *protocol.RequestFrame) {
	agents := r.server.agents.ListInfo()
	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"agents":  agents,
		"clients": len(r.server.clients),
	}))
}

