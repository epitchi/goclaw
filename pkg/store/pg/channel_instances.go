package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/crypto"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// PGChannelInstanceStore implements store.ChannelInstanceStore backed by Postgres.
type PGChannelInstanceStore struct {
	db     *sql.DB
	encKey string
}

func NewPGChannelInstanceStore(db *sql.DB, encryptionKey string) *PGChannelInstanceStore {
	return &PGChannelInstanceStore{db: db, encKey: encryptionKey}
}

const channelInstanceSelectCols = `id, name, display_name, channel_type, agent_id,
 credentials, config, enabled, created_by, created_at, updated_at`

func (s *PGChannelInstanceStore) Create(ctx context.Context, inst *store.ChannelInstanceData) error {
	if err := store.ValidateUserID(inst.CreatedBy); err != nil {
		return err
	}
	if inst.ID == uuid.Nil {
		inst.ID = store.GenNewID()
	}

	// Encrypt credentials if provided
	var credsBytes []byte
	if len(inst.Credentials) > 0 && s.encKey != "" {
		encrypted, err := crypto.Encrypt(string(inst.Credentials), s.encKey)
		if err != nil {
			return fmt.Errorf("encrypt credentials: %w", err)
		}
		credsBytes = []byte(encrypted)
	} else {
		credsBytes = inst.Credentials
	}

	now := time.Now()
	inst.CreatedAt = now
	inst.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_instances (id, name, display_name, channel_type, agent_id,
		 credentials, config, enabled, created_by, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		inst.ID, inst.Name, inst.DisplayName, inst.ChannelType, inst.AgentID,
		credsBytes, jsonOrEmpty(inst.Config),
		inst.Enabled, inst.CreatedBy, now, now,
	)
	return err
}

func (s *PGChannelInstanceStore) Get(ctx context.Context, id uuid.UUID) (*store.ChannelInstanceData, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelInstanceSelectCols+` FROM channel_instances WHERE id = $1`, id)
	return s.scanInstance(row)
}

func (s *PGChannelInstanceStore) GetByName(ctx context.Context, name string) (*store.ChannelInstanceData, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelInstanceSelectCols+` FROM channel_instances WHERE name = $1`, name)
	return s.scanInstance(row)
}

func (s *PGChannelInstanceStore) scanInstance(row *sql.Row) (*store.ChannelInstanceData, error) {
	var inst store.ChannelInstanceData
	var displayName *string
	var creds []byte
	var config *[]byte

	err := row.Scan(
		&inst.ID, &inst.Name, &displayName, &inst.ChannelType, &inst.AgentID,
		&creds, &config,
		&inst.Enabled, &inst.CreatedBy, &inst.CreatedAt, &inst.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	inst.DisplayName = derefStr(displayName)
	if config != nil {
		inst.Config = *config
	}

	// Decrypt credentials
	if len(creds) > 0 && s.encKey != "" {
		decrypted, err := crypto.Decrypt(string(creds), s.encKey)
		if err != nil {
			slog.Warn("channel_instances: failed to decrypt credentials", "name", inst.Name, "error", err)
		} else {
			inst.Credentials = []byte(decrypted)
		}
	} else {
		inst.Credentials = creds
	}

	return &inst, nil
}

func (s *PGChannelInstanceStore) scanInstances(rows *sql.Rows) ([]store.ChannelInstanceData, error) {
	defer rows.Close()
	var result []store.ChannelInstanceData
	for rows.Next() {
		var inst store.ChannelInstanceData
		var displayName *string
		var creds []byte
		var config *[]byte

		if err := rows.Scan(
			&inst.ID, &inst.Name, &displayName, &inst.ChannelType, &inst.AgentID,
			&creds, &config,
			&inst.Enabled, &inst.CreatedBy, &inst.CreatedAt, &inst.UpdatedAt,
		); err != nil {
			continue
		}

		inst.DisplayName = derefStr(displayName)
		if config != nil {
			inst.Config = *config
		}
		if len(creds) > 0 && s.encKey != "" {
			if decrypted, err := crypto.Decrypt(string(creds), s.encKey); err == nil {
				inst.Credentials = []byte(decrypted)
			}
		} else {
			inst.Credentials = creds
		}

		result = append(result, inst)
	}
	return result, nil
}

func (s *PGChannelInstanceStore) Update(ctx context.Context, id uuid.UUID, updates map[string]any) error {
	// Encrypt credentials if present
	if credsVal, ok := updates["credentials"]; ok && credsVal != nil {
		var credsStr string
		switch v := credsVal.(type) {
		case string:
			credsStr = v
		default:
			// Object/map from JSON — marshal to string for encryption
			if b, err := json.Marshal(v); err == nil {
				credsStr = string(b)
			}
		}
		if credsStr != "" && s.encKey != "" {
			encrypted, err := crypto.Encrypt(credsStr, s.encKey)
			if err != nil {
				return fmt.Errorf("encrypt credentials: %w", err)
			}
			updates["credentials"] = []byte(encrypted)
		}
	}
	updates["updated_at"] = time.Now()
	return execMapUpdate(ctx, s.db, "channel_instances", id, updates)
}

func (s *PGChannelInstanceStore) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM channel_instances WHERE id = $1", id)
	return err
}

func (s *PGChannelInstanceStore) ListEnabled(ctx context.Context) ([]store.ChannelInstanceData, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelInstanceSelectCols+` FROM channel_instances WHERE enabled = true ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return s.scanInstances(rows)
}

func (s *PGChannelInstanceStore) ListAll(ctx context.Context) ([]store.ChannelInstanceData, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelInstanceSelectCols+` FROM channel_instances ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return s.scanInstances(rows)
}

func buildChannelInstanceWhere(opts store.ChannelInstanceListOpts) (string, []interface{}) {
	var conditions []string
	var args []interface{}
	argIdx := 1

	if opts.Search != "" {
		conditions = append(conditions, fmt.Sprintf("(name ILIKE $%d OR display_name ILIKE $%d OR channel_type ILIKE $%d)", argIdx, argIdx, argIdx))
		args = append(args, "%"+opts.Search+"%")
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	return where, args
}

func (s *PGChannelInstanceStore) ListPaged(ctx context.Context, opts store.ChannelInstanceListOpts) ([]store.ChannelInstanceData, error) {
	where, args := buildChannelInstanceWhere(opts)
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + channelInstanceSelectCols + ` FROM channel_instances` + where +
		fmt.Sprintf(" ORDER BY name OFFSET %d LIMIT %d", opts.Offset, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return s.scanInstances(rows)
}

func (s *PGChannelInstanceStore) CountInstances(ctx context.Context, opts store.ChannelInstanceListOpts) (int, error) {
	where, args := buildChannelInstanceWhere(opts)
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM channel_instances"+where, args...).Scan(&count)
	return count, err
}
