package file

import (
	"github.com/nextlevelbuilder/goclaw/pkg/pairing"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// FilePairingStore wraps pairing.Service to implement store.PairingStore.
type FilePairingStore struct {
	svc *pairing.Service
}

func NewFilePairingStore(svc *pairing.Service) *FilePairingStore {
	return &FilePairingStore{svc: svc}
}

// Service returns the underlying pairing.Service for direct access during migration.
func (f *FilePairingStore) Service() *pairing.Service { return f.svc }

func (f *FilePairingStore) RequestPairing(senderID, channel, chatID, accountID string) (string, error) {
	return f.svc.RequestPairing(senderID, channel, chatID, accountID)
}

func (f *FilePairingStore) ApprovePairing(code, approvedBy string) (*store.PairedDeviceData, error) {
	pd, err := f.svc.ApprovePairing(code, approvedBy)
	if err != nil {
		return nil, err
	}
	return &store.PairedDeviceData{
		SenderID: pd.SenderID,
		Channel:  pd.Channel,
		ChatID:   pd.ChatID,
		PairedAt: pd.PairedAt,
		PairedBy: pd.PairedBy,
	}, nil
}

func (f *FilePairingStore) DenyPairing(code string) error {
	return f.svc.DenyPairing(code)
}

func (f *FilePairingStore) RevokePairing(senderID, channel string) error {
	return f.svc.RevokePairing(senderID, channel)
}

func (f *FilePairingStore) IsPaired(senderID, channel string) bool {
	return f.svc.IsPaired(senderID, channel)
}

func (f *FilePairingStore) ListPending() []store.PairingRequestData {
	items := f.svc.ListPending()
	result := make([]store.PairingRequestData, len(items))
	for i, item := range items {
		result[i] = store.PairingRequestData{
			Code:      item.Code,
			SenderID:  item.SenderID,
			Channel:   item.Channel,
			ChatID:    item.ChatID,
			AccountID: item.AccountID,
			CreatedAt: item.CreatedAt,
			ExpiresAt: item.ExpiresAt,
		}
	}
	return result
}

func (f *FilePairingStore) ListPaired() []store.PairedDeviceData {
	items := f.svc.ListPaired()
	result := make([]store.PairedDeviceData, len(items))
	for i, item := range items {
		result[i] = store.PairedDeviceData{
			SenderID: item.SenderID,
			Channel:  item.Channel,
			ChatID:   item.ChatID,
			PairedAt: item.PairedAt,
			PairedBy: item.PairedBy,
		}
	}
	return result
}
