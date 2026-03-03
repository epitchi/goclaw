// Package pairing implements the DM/device pairing system.
//
// When a new user sends a DM (and DM policy is "pairing"), the system:
//  1. Generates an 8-character alphanumeric pairing code
//  2. Notifies the owner about the pairing request
//  3. Owner approves by running "/pair CODE" in any connected channel
//  4. Sender is added to the channel's allowlist
//
// Pairing codes use the alphabet ABCDEFGHJKLMNPQRSTUVWXYZ23456789
// (no ambiguous characters: 0, O, 1, I, L).
// Codes expire after 60 minutes. Max 3 pending codes per account.
package pairing

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// CodeAlphabet excludes ambiguous characters (0, O, 1, I, L).
	CodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// CodeLength is the number of characters in a pairing code.
	CodeLength = 8
	// CodeTTL is how long a pairing code remains valid.
	CodeTTL = 60 * time.Minute
	// MaxPendingPerAccount is the max number of pending codes per account.
	MaxPendingPerAccount = 3
)

// PairingRequest represents a pending pairing code.
type PairingRequest struct {
	Code      string `json:"code"`
	SenderID  string `json:"sender_id"`
	Channel   string `json:"channel"`
	ChatID    string `json:"chat_id"`
	AccountID string `json:"account_id"`
	CreatedAt int64  `json:"created_at"` // unix millis
	ExpiresAt int64  `json:"expires_at"` // unix millis
}

// PairedDevice represents an approved pairing.
type PairedDevice struct {
	SenderID  string `json:"sender_id"`
	Channel   string `json:"channel"`
	ChatID    string `json:"chat_id"`
	PairedAt  int64  `json:"paired_at"` // unix millis
	PairedBy  string `json:"paired_by"` // who approved
}

// Store is the persistent store for pairing data.
type Store struct {
	Pending []PairingRequest `json:"pending"`
	Paired  []PairedDevice   `json:"paired"`
}

// Service manages pairing codes and approved devices.
type Service struct {
	storePath string
	store     Store
	mu        sync.Mutex
}

// NewService creates a new pairing service.
// storePath is the path to the JSON file for persistence (e.g., ~/.goclaw/data/pairing.json).
func NewService(storePath string) *Service {
	s := &Service{
		storePath: storePath,
	}
	s.load()
	return s
}

// RequestPairing generates a new pairing code for a sender.
// Returns the generated code or an error if max pending codes exceeded.
func (s *Service) RequestPairing(senderID, channel, chatID, accountID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Prune expired
	s.pruneExpired()

	// Check max pending per account
	count := 0
	for _, req := range s.store.Pending {
		if req.AccountID == accountID {
			count++
		}
	}
	if count >= MaxPendingPerAccount {
		return "", fmt.Errorf("max pending pairing requests (%d) exceeded for account %s", MaxPendingPerAccount, accountID)
	}

	// Check if sender already has a pending code
	for _, req := range s.store.Pending {
		if req.SenderID == senderID && req.Channel == channel {
			return req.Code, nil // return existing code
		}
	}

	code := generateCode()
	now := time.Now()

	req := PairingRequest{
		Code:      code,
		SenderID:  senderID,
		Channel:   channel,
		ChatID:    chatID,
		AccountID: accountID,
		CreatedAt: now.UnixMilli(),
		ExpiresAt: now.Add(CodeTTL).UnixMilli(),
	}

	s.store.Pending = append(s.store.Pending, req)
	s.save()

	slog.Info("pairing code generated",
		"code", code,
		"sender", senderID,
		"channel", channel,
	)

	return code, nil
}

// ApprovePairing validates a code and moves the sender to the paired list.
// Returns the paired device info or an error if code is invalid/expired.
func (s *Service) ApprovePairing(code, approvedBy string) (*PairedDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpired()

	for i, req := range s.store.Pending {
		if req.Code != code {
			continue
		}

		// Remove from pending
		s.store.Pending = append(s.store.Pending[:i], s.store.Pending[i+1:]...)

		// Add to paired
		paired := PairedDevice{
			SenderID: req.SenderID,
			Channel:  req.Channel,
			ChatID:   req.ChatID,
			PairedAt: time.Now().UnixMilli(),
			PairedBy: approvedBy,
		}
		s.store.Paired = append(s.store.Paired, paired)
		s.save()

		slog.Info("pairing approved",
			"sender", req.SenderID,
			"channel", req.Channel,
			"approved_by", approvedBy,
		)

		return &paired, nil
	}

	return nil, fmt.Errorf("pairing code %s not found or expired", code)
}

// DenyPairing removes a pending pairing request by code.
func (s *Service) DenyPairing(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, req := range s.store.Pending {
		if req.Code == code {
			s.store.Pending = append(s.store.Pending[:i], s.store.Pending[i+1:]...)
			s.save()
			slog.Info("pairing denied", "code", code, "sender", req.SenderID, "channel", req.Channel)
			return nil
		}
	}
	return fmt.Errorf("pairing code %s not found or expired", code)
}

// RevokePairing removes a paired device.
func (s *Service) RevokePairing(senderID, channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, p := range s.store.Paired {
		if p.SenderID == senderID && p.Channel == channel {
			s.store.Paired = append(s.store.Paired[:i], s.store.Paired[i+1:]...)
			s.save()
			slog.Info("pairing revoked", "sender", senderID, "channel", channel)
			return nil
		}
	}
	return fmt.Errorf("paired device not found: %s/%s", channel, senderID)
}

// IsPaired checks if a sender is paired for a channel.
func (s *Service) IsPaired(senderID, channel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range s.store.Paired {
		if p.SenderID == senderID && p.Channel == channel {
			return true
		}
	}
	return false
}

// ListPending returns all pending pairing requests.
func (s *Service) ListPending() []PairingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneExpired()

	result := make([]PairingRequest, len(s.store.Pending))
	copy(result, s.store.Pending)
	return result
}

// ListPaired returns all paired devices.
func (s *Service) ListPaired() []PairedDevice {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]PairedDevice, len(s.store.Paired))
	copy(result, s.store.Paired)
	return result
}

// --- Internal ---

func (s *Service) pruneExpired() {
	now := time.Now().UnixMilli()
	var valid []PairingRequest
	for _, req := range s.store.Pending {
		if req.ExpiresAt > now {
			valid = append(valid, req)
		}
	}
	s.store.Pending = valid
}

func (s *Service) load() {
	data, err := os.ReadFile(s.storePath)
	if err != nil {
		return // file doesn't exist yet
	}
	json.Unmarshal(data, &s.store)
}

func (s *Service) save() {
	dir := filepath.Dir(s.storePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Error("pairing: failed to create dir", "error", err)
		return
	}
	data, err := json.MarshalIndent(s.store, "", "  ")
	if err != nil {
		slog.Error("pairing: failed to marshal store", "error", err)
		return
	}
	if err := os.WriteFile(s.storePath, data, 0600); err != nil {
		slog.Error("pairing: failed to write store", "error", err)
	}
}

func generateCode() string {
	b := make([]byte, CodeLength)
	rand.Read(b)
	code := make([]byte, CodeLength)
	for i := range code {
		code[i] = CodeAlphabet[int(b[i])%len(CodeAlphabet)]
	}
	return string(code)
}
