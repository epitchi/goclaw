package file

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/nextlevelbuilder/goclaw/pkg/bootstrap"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// standaloneNS is the UUID v5 namespace for standalone agent IDs.
// Deterministic: same agent key always produces the same UUID.
var standaloneNS = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

// AgentUUID generates a deterministic UUID v5 from an agent key.
func AgentUUID(key string) uuid.UUID {
	return uuid.NewSHA1(standaloneNS, []byte("goclaw-standalone:"+key))
}

// agentContextFiles lists filenames read from the workspace root for agent-level context.
var agentContextFiles = []string{
	bootstrap.AgentsFile,
	bootstrap.SoulFile,
	bootstrap.ToolsFile,
	bootstrap.IdentityFile,
	bootstrap.HeartbeatFile,
	bootstrap.BootstrapFile,
}

// AgentEntry describes an agent to register in the FileAgentStore.
type AgentEntry struct {
	Key       string
	AgentType string // "open" or "predefined"
	Workspace string // absolute path
}

// agentEntry holds in-memory agent metadata.
type agentEntry struct {
	data      *store.AgentData
	workspace string
}

// FileAgentStore implements store.AgentStore for standalone mode.
// Agent metadata is in-memory (from config). Per-user context files,
// user profiles, and group file writers are in SQLite.
// Agent-level context files are on the filesystem.
type FileAgentStore struct {
	agents map[string]*agentEntry    // agentKey → entry
	byID   map[uuid.UUID]*agentEntry // agentUUID → entry
	db     *sql.DB
	mu     sync.RWMutex
}

// NewFileAgentStore creates a FileAgentStore backed by SQLite at dbPath.
// Entries define the agents available (built from config).
func NewFileAgentStore(dbPath string, entries []AgentEntry) (*FileAgentStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := createTables(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &FileAgentStore{
		agents: make(map[string]*agentEntry, len(entries)),
		byID:   make(map[uuid.UUID]*agentEntry, len(entries)),
		db:     db,
	}

	for _, e := range entries {
		id := AgentUUID(e.Key)
		agentType := e.AgentType
		if agentType == "" {
			agentType = store.AgentTypeOpen
		}
		entry := &agentEntry{
			data: &store.AgentData{
				BaseModel: store.BaseModel{
					ID:        id,
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				},
				AgentKey:  e.Key,
				AgentType: agentType,
				Workspace: e.Workspace,
				Status:    store.AgentStatusActive,
			},
			workspace: e.Workspace,
		}
		s.agents[e.Key] = entry
		s.byID[id] = entry
	}

	return s, nil
}

// Close closes the underlying SQLite database.
func (s *FileAgentStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func createTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS user_context_files (
			agent_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			file_name TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, user_id, file_name)
		);
		CREATE TABLE IF NOT EXISTS user_profiles (
			agent_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			workspace TEXT DEFAULT '',
			first_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, user_id)
		);
		CREATE TABLE IF NOT EXISTS group_file_writers (
			agent_id TEXT NOT NULL,
			group_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			display_name TEXT DEFAULT '',
			username TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, group_id, user_id)
		);
	`)
	return err
}

// --- Agent lookup (in-memory from config) ---

func (s *FileAgentStore) GetByKey(_ context.Context, agentKey string) (*store.AgentData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.agents[agentKey]
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", agentKey)
	}
	return entry.data, nil
}

func (s *FileAgentStore) GetByID(_ context.Context, id uuid.UUID) (*store.AgentData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", id)
	}
	return entry.data, nil
}

// --- Agent-level context files (filesystem) ---

func (s *FileAgentStore) GetAgentContextFiles(_ context.Context, agentID uuid.UUID) ([]store.AgentContextFileData, error) {
	s.mu.RLock()
	entry, ok := s.byID[agentID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", agentID)
	}

	var files []store.AgentContextFileData
	for _, name := range agentContextFiles {
		path := filepath.Join(entry.workspace, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.Warn("failed to read agent context file", "file", name, "error", err)
			continue
		}
		files = append(files, store.AgentContextFileData{
			AgentID:  agentID,
			FileName: name,
			Content:  string(data),
		})
	}
	return files, nil
}

func (s *FileAgentStore) SetAgentContextFile(_ context.Context, agentID uuid.UUID, fileName, content string) error {
	s.mu.RLock()
	entry, ok := s.byID[agentID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	path := filepath.Join(entry.workspace, fileName)
	return os.WriteFile(path, []byte(content), 0644)
}

// --- Per-user context files (SQLite) ---

func (s *FileAgentStore) GetUserContextFiles(_ context.Context, agentID uuid.UUID, userID string) ([]store.UserContextFileData, error) {
	rows, err := s.db.Query(
		`SELECT agent_id, user_id, file_name, content FROM user_context_files WHERE agent_id = ? AND user_id = ?`,
		agentID.String(), userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []store.UserContextFileData
	for rows.Next() {
		var agentIDStr, uid, fileName, content string
		if err := rows.Scan(&agentIDStr, &uid, &fileName, &content); err != nil {
			return nil, err
		}
		files = append(files, store.UserContextFileData{
			AgentID:  agentID,
			UserID:   uid,
			FileName: fileName,
			Content:  content,
		})
	}
	return files, rows.Err()
}

func (s *FileAgentStore) SetUserContextFile(_ context.Context, agentID uuid.UUID, userID, fileName, content string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_context_files (agent_id, user_id, file_name, content, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(agent_id, user_id, file_name) DO UPDATE SET content = excluded.content, updated_at = CURRENT_TIMESTAMP`,
		agentID.String(), userID, fileName, content,
	)
	return err
}

func (s *FileAgentStore) DeleteUserContextFile(_ context.Context, agentID uuid.UUID, userID, fileName string) error {
	_, err := s.db.Exec(
		`DELETE FROM user_context_files WHERE agent_id = ? AND user_id = ? AND file_name = ?`,
		agentID.String(), userID, fileName,
	)
	return err
}

// --- User profiles (SQLite) ---

func (s *FileAgentStore) GetOrCreateUserProfile(_ context.Context, agentID uuid.UUID, userID, workspace, channel string) (bool, string, error) {
	// Build workspace with channel segment for isolation.
	effectiveWs := workspace
	if channel != "" {
		effectiveWs = filepath.Join(workspace, channel)
	}

	result, err := s.db.Exec(
		`INSERT OR IGNORE INTO user_profiles (agent_id, user_id, workspace) VALUES (?, ?, ?)`,
		agentID.String(), userID, effectiveWs,
	)
	if err != nil {
		return false, effectiveWs, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, effectiveWs, err
	}
	if affected > 0 {
		return true, effectiveWs, nil // new profile
	}

	// Existing profile — update last_seen, return stored workspace
	var storedWs sql.NullString
	_ = s.db.QueryRow(
		`UPDATE user_profiles SET last_seen_at = CURRENT_TIMESTAMP WHERE agent_id = ? AND user_id = ? RETURNING workspace`,
		agentID.String(), userID,
	).Scan(&storedWs)
	ws := effectiveWs
	if storedWs.Valid && storedWs.String != "" {
		ws = storedWs.String
	}
	return false, ws, nil
}

// --- Group file writers (SQLite) ---

func (s *FileAgentStore) IsGroupFileWriter(_ context.Context, agentID uuid.UUID, groupID, userID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM group_file_writers WHERE agent_id = ? AND group_id = ? AND user_id = ?`,
		agentID.String(), groupID, userID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *FileAgentStore) AddGroupFileWriter(_ context.Context, agentID uuid.UUID, groupID, userID, displayName, username string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO group_file_writers (agent_id, group_id, user_id, display_name, username) VALUES (?, ?, ?, ?, ?)`,
		agentID.String(), groupID, userID, displayName, username,
	)
	return err
}

func (s *FileAgentStore) RemoveGroupFileWriter(_ context.Context, agentID uuid.UUID, groupID, userID string) error {
	_, err := s.db.Exec(
		`DELETE FROM group_file_writers WHERE agent_id = ? AND group_id = ? AND user_id = ?`,
		agentID.String(), groupID, userID,
	)
	return err
}

func (s *FileAgentStore) ListGroupFileWriterGroups(_ context.Context, agentID uuid.UUID) ([]store.GroupWriterGroupInfo, error) {
	rows, err := s.db.Query(
		`SELECT group_id, COUNT(*) as writer_count FROM group_file_writers WHERE agent_id = ? GROUP BY group_id ORDER BY group_id`,
		agentID.String(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []store.GroupWriterGroupInfo
	for rows.Next() {
		var g store.GroupWriterGroupInfo
		if err := rows.Scan(&g.GroupID, &g.WriterCount); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *FileAgentStore) ListGroupFileWriters(_ context.Context, agentID uuid.UUID, groupID string) ([]store.GroupFileWriterData, error) {
	rows, err := s.db.Query(
		`SELECT user_id, display_name, username FROM group_file_writers WHERE agent_id = ? AND group_id = ?`,
		agentID.String(), groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var writers []store.GroupFileWriterData
	for rows.Next() {
		var userID string
		var displayName, username sql.NullString
		if err := rows.Scan(&userID, &displayName, &username); err != nil {
			return nil, err
		}
		w := store.GroupFileWriterData{UserID: userID}
		if displayName.Valid {
			w.DisplayName = &displayName.String
		}
		if username.Valid {
			w.Username = &username.String
		}
		writers = append(writers, w)
	}
	return writers, rows.Err()
}

// --- User overrides (not supported in standalone) ---

func (s *FileAgentStore) GetUserOverride(_ context.Context, _ uuid.UUID, _ string) (*store.UserAgentOverrideData, error) {
	return nil, nil
}

func (s *FileAgentStore) SetUserOverride(_ context.Context, _ *store.UserAgentOverrideData) error {
	return fmt.Errorf("user overrides not supported in standalone mode")
}

// --- CRUD / access control (not supported in standalone) ---

func (s *FileAgentStore) Create(_ context.Context, _ *store.AgentData) error {
	return fmt.Errorf("agent CRUD not supported in standalone mode")
}

func (s *FileAgentStore) Update(_ context.Context, _ uuid.UUID, _ map[string]any) error {
	return fmt.Errorf("agent CRUD not supported in standalone mode")
}

func (s *FileAgentStore) Delete(_ context.Context, _ uuid.UUID) error {
	return fmt.Errorf("agent CRUD not supported in standalone mode")
}

func (s *FileAgentStore) List(_ context.Context, _ string) ([]store.AgentData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var agents []store.AgentData
	for _, entry := range s.agents {
		agents = append(agents, *entry.data)
	}
	return agents, nil
}

func (s *FileAgentStore) GetDefault(_ context.Context) (*store.AgentData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.agents {
		return entry.data, nil
	}
	return nil, fmt.Errorf("no agents configured")
}

func (s *FileAgentStore) ShareAgent(_ context.Context, _ uuid.UUID, _, _, _ string) error {
	return fmt.Errorf("agent sharing not supported in standalone mode")
}

func (s *FileAgentStore) RevokeShare(_ context.Context, _ uuid.UUID, _ string) error {
	return fmt.Errorf("agent sharing not supported in standalone mode")
}

func (s *FileAgentStore) ListShares(_ context.Context, _ uuid.UUID) ([]store.AgentShareData, error) {
	return nil, nil
}

func (s *FileAgentStore) CanAccess(_ context.Context, _ uuid.UUID, _ string) (bool, string, error) {
	return true, "owner", nil // standalone: all access allowed
}

func (s *FileAgentStore) ListAccessible(_ context.Context, _ string) ([]store.AgentData, error) {
	return s.List(context.Background(), "")
}
