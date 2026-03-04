package whatsapp

import (
	"encoding/json"
	"fmt"

	"github.com/epitchi/goclaw/pkg/bus"
	"github.com/epitchi/goclaw/pkg/channels"
	"github.com/epitchi/goclaw/pkg/config"
	"github.com/epitchi/goclaw/pkg/store"
)

// whatsappCreds maps the credentials JSON from the channel_instances table.
type whatsappCreds struct {
	BridgeURL string `json:"bridge_url"`
}

// whatsappInstanceConfig maps the non-secret config JSONB from the channel_instances table.
type whatsappInstanceConfig struct {
	DMPolicy    string   `json:"dm_policy,omitempty"`
	GroupPolicy string   `json:"group_policy,omitempty"`
	AllowFrom   []string `json:"allow_from,omitempty"`
}

// Factory creates a WhatsApp channel from DB instance data.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

	var c whatsappCreds
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &c); err != nil {
			return nil, fmt.Errorf("decode whatsapp credentials: %w", err)
		}
	}
	if c.BridgeURL == "" {
		return nil, fmt.Errorf("whatsapp bridge_url is required")
	}

	var ic whatsappInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("decode whatsapp config: %w", err)
		}
	}

	waCfg := config.WhatsAppConfig{
		Enabled:     true,
		BridgeURL:   c.BridgeURL,
		AllowFrom:   ic.AllowFrom,
		DMPolicy:    ic.DMPolicy,
		GroupPolicy: ic.GroupPolicy,
	}

	// DB instances default to "pairing" for groups (secure by default).
	if waCfg.GroupPolicy == "" {
		waCfg.GroupPolicy = "pairing"
	}

	ch, err := New(waCfg, msgBus, pairingSvc)
	if err != nil {
		return nil, err
	}

	ch.SetName(name)
	return ch, nil
}
