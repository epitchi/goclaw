package zalomethods

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/bus"
	"github.com/epitchi/goclaw/pkg/channels/zalo/personal/protocol"
	"github.com/epitchi/goclaw/pkg/gateway"
	"github.com/epitchi/goclaw/pkg/store"
	goclawprotocol "github.com/epitchi/goclaw/pkg/protocol"
)

// QRMethods handles QR login for zalo_personal channel instances.
type QRMethods struct {
	instanceStore  store.ChannelInstanceStore
	msgBus         *bus.MessageBus
	activeSessions sync.Map // instanceID (string) -> struct{}
}

func NewQRMethods(s store.ChannelInstanceStore, msgBus *bus.MessageBus) *QRMethods {
	return &QRMethods{instanceStore: s, msgBus: msgBus}
}

func (m *QRMethods) Register(router *gateway.MethodRouter) {
	router.Register(goclawprotocol.MethodZaloPersonalQRStart, m.handleQRStart)
}

func (m *QRMethods) handleQRStart(ctx context.Context, client *gateway.Client, req *goclawprotocol.RequestFrame) {
	var params struct {
		InstanceID string `json:"instance_id"`
	}
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
	}

	instID, err := uuid.Parse(params.InstanceID)
	if err != nil {
		client.SendResponse(goclawprotocol.NewErrorResponse(req.ID, goclawprotocol.ErrInvalidRequest, "invalid instance_id"))
		return
	}

	inst, err := m.instanceStore.Get(ctx, instID)
	if err != nil || inst.ChannelType != "zalo_personal" {
		client.SendResponse(goclawprotocol.NewErrorResponse(req.ID, goclawprotocol.ErrNotFound, "zalo_personal instance not found"))
		return
	}

	if _, loaded := m.activeSessions.LoadOrStore(params.InstanceID, struct{}{}); loaded {
		client.SendResponse(goclawprotocol.NewErrorResponse(req.ID, goclawprotocol.ErrInvalidRequest, "QR session already active for this instance"))
		return
	}

	// ACK immediately — QR arrives via event.
	client.SendResponse(goclawprotocol.NewOKResponse(req.ID, map[string]any{"status": "started"}))

	go m.runQRFlow(ctx, client, params.InstanceID, instID)
}

func (m *QRMethods) runQRFlow(ctx context.Context, client *gateway.Client, instanceIDStr string, instanceID uuid.UUID) {
	defer m.activeSessions.Delete(instanceIDStr)

	sess := protocol.NewSession()
	// LoginQR has internal 100s timeout per QR code. Use 2m as outer bound.
	// Derived from parent ctx so QR flow cancels when the WS client disconnects.
	qrCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cred, err := protocol.LoginQR(qrCtx, sess, func(qrPNG []byte) {
		client.SendEvent(goclawprotocol.EventFrame{
			Type:  goclawprotocol.FrameTypeEvent,
			Event: goclawprotocol.EventZaloPersonalQRCode,
			Payload: map[string]any{
				"instance_id": instanceIDStr,
				"png_b64":     base64.StdEncoding.EncodeToString(qrPNG),
			},
		})
	})

	if err != nil {
		slog.Warn("Zalo Personal QR login failed", "instance", instanceIDStr, "error", err)
		client.SendEvent(*goclawprotocol.NewEvent(goclawprotocol.EventZaloPersonalQRDone, map[string]any{
			"instance_id": instanceIDStr,
			"success":     false,
			"error":       err.Error(),
		}))
		return
	}

	credsJSON, err := json.Marshal(map[string]any{
		"imei":      cred.IMEI,
		"cookie":    cred.Cookie,
		"userAgent": cred.UserAgent,
		"language":  cred.Language,
	})
	if err != nil {
		slog.Error("Zalo Personal QR: marshal credentials failed", "error", err)
		client.SendEvent(*goclawprotocol.NewEvent(goclawprotocol.EventZaloPersonalQRDone, map[string]any{
			"instance_id": instanceIDStr,
			"success":     false,
			"error":       "internal error: credential serialization failed",
		}))
		return
	}

	if err := m.instanceStore.Update(context.Background(), instanceID, map[string]any{
		"credentials": string(credsJSON),
	}); err != nil {
		slog.Error("Zalo Personal QR: save credentials failed", "instance", instanceIDStr, "error", err)
		client.SendEvent(*goclawprotocol.NewEvent(goclawprotocol.EventZaloPersonalQRDone, map[string]any{
			"instance_id": instanceIDStr,
			"success":     false,
			"error":       "failed to save credentials",
		}))
		return
	}

	// Trigger instanceLoader reload via cache invalidation.
	if m.msgBus != nil {
		m.msgBus.Broadcast(bus.Event{
			Name:    goclawprotocol.EventCacheInvalidate,
			Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindChannelInstances},
		})
	}

	client.SendEvent(*goclawprotocol.NewEvent(goclawprotocol.EventZaloPersonalQRDone, map[string]any{
		"instance_id": instanceIDStr,
		"success":     true,
	}))

	slog.Info("Zalo Personal QR login completed, credentials saved", "instance", instanceIDStr)
}
