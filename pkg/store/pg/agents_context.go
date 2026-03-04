package pg

import (
	"context"
	"database/sql"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/config"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// --- Agent-level Context Files ---

func (s *PGAgentStore) GetAgentContextFiles(ctx context.Context, agentID uuid.UUID) ([]store.AgentContextFileData, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT agent_id, file_name, content FROM agent_context_files WHERE agent_id = $1", agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []store.AgentContextFileData
	for rows.Next() {
		var d store.AgentContextFileData
		if err := rows.Scan(&d.AgentID, &d.FileName, &d.Content); err != nil {
			continue
		}
		result = append(result, d)
	}
	return result, nil
}

func (s *PGAgentStore) SetAgentContextFile(ctx context.Context, agentID uuid.UUID, fileName, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_context_files (id, agent_id, file_name, content, updated_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (agent_id, file_name) DO UPDATE SET content = EXCLUDED.content, updated_at = EXCLUDED.updated_at`,
		store.GenNewID(), agentID, fileName, content, time.Now(),
	)
	return err
}

// --- Per-user Context Files ---

func (s *PGAgentStore) GetUserContextFiles(ctx context.Context, agentID uuid.UUID, userID string) ([]store.UserContextFileData, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT agent_id, user_id, file_name, content FROM user_context_files WHERE agent_id = $1 AND user_id = $2", agentID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []store.UserContextFileData
	for rows.Next() {
		var d store.UserContextFileData
		if err := rows.Scan(&d.AgentID, &d.UserID, &d.FileName, &d.Content); err != nil {
			continue
		}
		result = append(result, d)
	}
	return result, nil
}

func (s *PGAgentStore) SetUserContextFile(ctx context.Context, agentID uuid.UUID, userID, fileName, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_context_files (id, agent_id, user_id, file_name, content, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (agent_id, user_id, file_name) DO UPDATE SET content = EXCLUDED.content, updated_at = EXCLUDED.updated_at`,
		store.GenNewID(), agentID, userID, fileName, content, time.Now(),
	)
	return err
}

func (s *PGAgentStore) DeleteUserContextFile(ctx context.Context, agentID uuid.UUID, userID, fileName string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM user_context_files WHERE agent_id = $1 AND user_id = $2 AND file_name = $3",
		agentID, userID, fileName)
	return err
}

// --- User-Agent Profiles ---

func (s *PGAgentStore) GetOrCreateUserProfile(ctx context.Context, agentID uuid.UUID, userID, workspace, channel string) (bool, string, error) {
	// Build workspace with channel segment for isolation.
	// Store in portable ~ form (e.g. "~/.goclaw/agent-ws/telegram").
	effectiveWs := config.ContractHome(workspace)
	if channel != "" {
		effectiveWs = filepath.Join(effectiveWs, channel)
	}

	var isInserted bool
	var storedWorkspace sql.NullString
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO user_agent_profiles (agent_id, user_id, workspace, first_seen_at, last_seen_at)
		VALUES ($1, $2, NULLIF($3, ''), NOW(), NOW())
		ON CONFLICT (agent_id, user_id) DO UPDATE SET last_seen_at = NOW()
		RETURNING (xmax = 0), workspace
	`, agentID, userID, effectiveWs).Scan(&isInserted, &storedWorkspace)
	if err != nil {
		return false, effectiveWs, err
	}
	ws := effectiveWs
	if storedWorkspace.Valid && storedWorkspace.String != "" {
		ws = storedWorkspace.String
	}
	return isInserted, ws, nil
}

// --- User Overrides ---

func (s *PGAgentStore) GetUserOverride(ctx context.Context, agentID uuid.UUID, userID string) (*store.UserAgentOverrideData, error) {
	var d store.UserAgentOverrideData
	err := s.db.QueryRowContext(ctx,
		"SELECT agent_id, user_id, provider, model FROM user_agent_overrides WHERE agent_id = $1 AND user_id = $2",
		agentID, userID,
	).Scan(&d.AgentID, &d.UserID, &d.Provider, &d.Model)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // not found = no override
		}
		return nil, nil
	}
	return &d, nil
}

func (s *PGAgentStore) SetUserOverride(ctx context.Context, override *store.UserAgentOverrideData) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_agent_overrides (id, agent_id, user_id, provider, model)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (agent_id, user_id) DO UPDATE SET provider = EXCLUDED.provider, model = EXCLUDED.model`,
		store.GenNewID(), override.AgentID, override.UserID, override.Provider, override.Model,
	)
	return err
}
