package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// ============================================================
// Delegation History
// ============================================================

func (s *PGTeamStore) SaveDelegationHistory(ctx context.Context, record *store.DelegationHistoryData) error {
	if record.ID == uuid.Nil {
		record.ID = store.GenNewID()
	}
	now := time.Now()
	record.CreatedAt = now

	metadata, _ := json.Marshal(record.Metadata)
	if len(metadata) == 0 {
		metadata = []byte(`{}`)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO delegation_history (id, source_agent_id, target_agent_id, team_id, team_task_id, user_id, task, mode, status, result, error, iterations, trace_id, duration_ms, metadata, created_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		record.ID, record.SourceAgentID, record.TargetAgentID,
		record.TeamID, record.TeamTaskID,
		record.UserID, record.Task, record.Mode, record.Status,
		record.Result, record.Error, record.Iterations,
		record.TraceID, record.DurationMS, metadata, now, record.CompletedAt,
	)
	return err
}

func (s *PGTeamStore) ListDelegationHistory(ctx context.Context, opts store.DelegationHistoryListOpts) ([]store.DelegationHistoryData, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argN := 0

	nextArg := func(v any) string {
		argN++
		args = append(args, v)
		return fmt.Sprintf("$%d", argN)
	}

	if opts.SourceAgentID != nil {
		where += " AND d.source_agent_id = " + nextArg(*opts.SourceAgentID)
	}
	if opts.TargetAgentID != nil {
		where += " AND d.target_agent_id = " + nextArg(*opts.TargetAgentID)
	}
	if opts.TeamID != nil {
		where += " AND d.team_id = " + nextArg(*opts.TeamID)
	}
	if opts.UserID != "" {
		where += " AND d.user_id = " + nextArg(opts.UserID)
	}
	if opts.Status != "" {
		where += " AND d.status = " + nextArg(opts.Status)
	}

	// Count total
	var total int
	countSQL := "SELECT COUNT(*) FROM delegation_history d " + where
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Fetch rows
	limit := opts.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	query := fmt.Sprintf(
		`SELECT d.id, d.source_agent_id, d.target_agent_id, d.team_id, d.team_task_id,
		 d.user_id, d.task, d.mode, d.status, d.result, d.error, d.iterations,
		 d.trace_id, d.duration_ms, d.metadata, d.created_at, d.completed_at,
		 COALESCE(sa.agent_key, '') AS source_agent_key,
		 COALESCE(ta.agent_key, '') AS target_agent_key
		 FROM delegation_history d
		 LEFT JOIN agents sa ON sa.id = d.source_agent_id
		 LEFT JOIN agents ta ON ta.id = d.target_agent_id
		 %s
		 ORDER BY d.created_at DESC
		 LIMIT %s OFFSET %s`,
		where, nextArg(limit), nextArg(offset))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var records []store.DelegationHistoryData
	for rows.Next() {
		var d store.DelegationHistoryData
		var result, errStr sql.NullString
		var completedAt sql.NullTime
		var metadata json.RawMessage
		if err := rows.Scan(
			&d.ID, &d.SourceAgentID, &d.TargetAgentID, &d.TeamID, &d.TeamTaskID,
			&d.UserID, &d.Task, &d.Mode, &d.Status, &result, &errStr, &d.Iterations,
			&d.TraceID, &d.DurationMS, &metadata, &d.CreatedAt, &completedAt,
			&d.SourceAgentKey, &d.TargetAgentKey,
		); err != nil {
			return nil, 0, err
		}
		if result.Valid {
			d.Result = &result.String
		}
		if errStr.Valid {
			d.Error = &errStr.String
		}
		if completedAt.Valid {
			d.CompletedAt = &completedAt.Time
		}
		if len(metadata) > 0 && string(metadata) != "{}" {
			_ = json.Unmarshal(metadata, &d.Metadata)
		}
		records = append(records, d)
	}
	return records, total, rows.Err()
}

func (s *PGTeamStore) GetDelegationHistory(ctx context.Context, id uuid.UUID) (*store.DelegationHistoryData, error) {
	var d store.DelegationHistoryData
	var result, errStr sql.NullString
	var completedAt sql.NullTime
	var metadata json.RawMessage

	err := s.db.QueryRowContext(ctx,
		`SELECT d.id, d.source_agent_id, d.target_agent_id, d.team_id, d.team_task_id,
		 d.user_id, d.task, d.mode, d.status, d.result, d.error, d.iterations,
		 d.trace_id, d.duration_ms, d.metadata, d.created_at, d.completed_at,
		 COALESCE(sa.agent_key, '') AS source_agent_key,
		 COALESCE(ta.agent_key, '') AS target_agent_key
		 FROM delegation_history d
		 LEFT JOIN agents sa ON sa.id = d.source_agent_id
		 LEFT JOIN agents ta ON ta.id = d.target_agent_id
		 WHERE d.id = $1`, id).Scan(
		&d.ID, &d.SourceAgentID, &d.TargetAgentID, &d.TeamID, &d.TeamTaskID,
		&d.UserID, &d.Task, &d.Mode, &d.Status, &result, &errStr, &d.Iterations,
		&d.TraceID, &d.DurationMS, &metadata, &d.CreatedAt, &completedAt,
		&d.SourceAgentKey, &d.TargetAgentKey,
	)
	if err != nil {
		return nil, err
	}
	if result.Valid {
		d.Result = &result.String
	}
	if errStr.Valid {
		d.Error = &errStr.String
	}
	if completedAt.Valid {
		d.CompletedAt = &completedAt.Time
	}
	if len(metadata) > 0 && string(metadata) != "{}" {
		_ = json.Unmarshal(metadata, &d.Metadata)
	}
	return &d, nil
}
