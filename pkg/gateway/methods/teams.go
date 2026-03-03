package methods

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/pkg/agent"
	"github.com/nextlevelbuilder/goclaw/pkg/bus"
	"github.com/nextlevelbuilder/goclaw/pkg/gateway"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

// TeamsMethods handles teams.* RPC methods.
type TeamsMethods struct {
	teamStore   store.TeamStore
	agentStore  store.AgentStore
	linkStore   store.AgentLinkStore // for auto-creating bidirectional links
	agentRouter *agent.Router        // for cache invalidation
	msgBus      *bus.MessageBus      // for pub/sub cache invalidation
}

func NewTeamsMethods(teamStore store.TeamStore, agentStore store.AgentStore, linkStore store.AgentLinkStore, agentRouter *agent.Router, msgBus *bus.MessageBus) *TeamsMethods {
	return &TeamsMethods{teamStore: teamStore, agentStore: agentStore, linkStore: linkStore, agentRouter: agentRouter, msgBus: msgBus}
}

// emitTeamCacheInvalidate broadcasts a cache invalidation event for team data.
func (m *TeamsMethods) emitTeamCacheInvalidate() {
	if m.msgBus == nil {
		return
	}
	m.msgBus.Broadcast(bus.Event{
		Name:    protocol.EventCacheInvalidate,
		Payload: bus.CacheInvalidatePayload{Kind: bus.CacheKindTeam},
	})
}

func (m *TeamsMethods) Register(router *gateway.MethodRouter) {
	router.Register(protocol.MethodTeamsList, m.handleList)
	router.Register(protocol.MethodTeamsCreate, m.handleCreate)
	router.Register(protocol.MethodTeamsGet, m.handleGet)
	router.Register(protocol.MethodTeamsDelete, m.handleDelete)
	router.Register(protocol.MethodTeamsTaskList, m.handleTaskList)
	router.Register(protocol.MethodTeamsMembersAdd, m.handleAddMember)
	router.Register(protocol.MethodTeamsMembersRemove, m.handleRemoveMember)
	router.Register(protocol.MethodTeamsUpdate, m.handleUpdate)
	router.Register(protocol.MethodTeamsKnownUsers, m.handleKnownUsers)
}

// --- List ---

func (m *TeamsMethods) handleList(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	ctx := context.Background()
	teams, err := m.teamStore.ListTeams(ctx)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"teams": teams,
		"count": len(teams),
	}))
}

// --- Create ---

type teamsCreateParams struct {
	Name        string          `json:"name"`
	Lead        string          `json:"lead"`    // agent key or UUID
	Members     []string        `json:"members"` // agent keys or UUIDs
	Description string          `json:"description"`
	Settings    json.RawMessage `json:"settings"`
}

func (m *TeamsMethods) handleCreate(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsCreateParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.Name == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "name is required"))
		return
	}
	if params.Lead == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "lead is required"))
		return
	}

	// Resolve lead agent
	leadAgent, err := resolveAgentInfo(m.agentStore, params.Lead)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "lead agent: "+err.Error()))
		return
	}

	// Resolve member agents
	var memberAgents []*store.AgentData
	for _, memberKey := range params.Members {
		ag, err := resolveAgentInfo(m.agentStore, memberKey)
		if err != nil {
			client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "member agent "+memberKey+": "+err.Error()))
			return
		}
		memberAgents = append(memberAgents, ag)
	}

	ctx := context.Background()

	// Create team
	team := &store.TeamData{
		Name:        params.Name,
		LeadAgentID: leadAgent.ID,
		Description: params.Description,
		Status:      store.TeamStatusActive,
		Settings:    params.Settings,
		CreatedBy:   client.UserID(),
	}
	if err := m.teamStore.CreateTeam(ctx, team); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to create team: "+err.Error()))
		return
	}

	// Add lead as member with lead role
	if err := m.teamStore.AddMember(ctx, team.ID, leadAgent.ID, store.TeamRoleLead); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to add lead as member: "+err.Error()))
		return
	}

	// Add members
	for _, ag := range memberAgents {
		if ag.ID == leadAgent.ID {
			continue // lead already added
		}
		if err := m.teamStore.AddMember(ctx, team.ID, ag.ID, store.TeamRoleMember); err != nil {
			slog.Warn("teams.create: failed to add member", "agent", ag.AgentKey, "error", err)
		}
	}

	// Auto-create outbound agent_links from lead to each member.
	// Only the lead can delegate to members.
	if m.linkStore != nil {
		m.autoCreateTeamLinks(ctx, team.ID, leadAgent, memberAgents, client.UserID())
	}

	// Invalidate agent caches so TEAM.md gets injected
	if m.agentRouter != nil {
		m.agentRouter.InvalidateAgent(leadAgent.AgentKey)
		for _, ag := range memberAgents {
			m.agentRouter.InvalidateAgent(ag.AgentKey)
		}
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"team": team,
	}))
}

// --- Get ---

type teamsGetParams struct {
	TeamID string `json:"teamId"`
}

func (m *TeamsMethods) handleGet(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsGetParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.TeamID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId is required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}

	ctx := context.Background()
	team, err := m.teamStore.GetTeam(ctx, teamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}

	members, err := m.teamStore.ListMembers(ctx, teamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"team":    team,
		"members": members,
	}))
}

// --- Delete ---

type teamsDeleteParams struct {
	TeamID string `json:"teamId"`
}

func (m *TeamsMethods) handleDelete(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsDeleteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.TeamID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId is required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}

	ctx := context.Background()

	// Fetch members before deleting for cache invalidation
	members, _ := m.teamStore.ListMembers(ctx, teamID)

	if err := m.teamStore.DeleteTeam(ctx, teamID); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to delete team: "+err.Error()))
		return
	}

	// Invalidate agent caches
	if m.agentRouter != nil {
		for _, member := range members {
			m.agentRouter.InvalidateAgent(member.AgentKey)
		}
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{"ok": true}))
}

// --- Task List (admin view) ---

type teamsTaskListParams struct {
	TeamID string `json:"teamId"`
}

func (m *TeamsMethods) handleTaskList(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsTaskListParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.TeamID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId is required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}

	ctx := context.Background()
	tasks, err := m.teamStore.ListTasks(ctx, teamID, "newest", store.TeamTaskFilterAll, "")
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"tasks": tasks,
		"count": len(tasks),
	}))
}

// --- Add Member ---

type teamsAddMemberParams struct {
	TeamID string `json:"teamId"`
	Agent  string `json:"agent"` // agent key or UUID
}

func (m *TeamsMethods) handleAddMember(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsAddMemberParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}
	if params.TeamID == "" || params.Agent == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId and agent are required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}

	ctx := context.Background()

	// Validate team exists
	team, err := m.teamStore.GetTeam(ctx, teamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "team not found: "+err.Error()))
		return
	}

	// Resolve agent
	ag, err := resolveAgentInfo(m.agentStore, params.Agent)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "agent: "+err.Error()))
		return
	}

	// Prevent adding lead again
	if ag.ID == team.LeadAgentID {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "agent is already the team lead"))
		return
	}

	// Add member
	if err := m.teamStore.AddMember(ctx, teamID, ag.ID, store.TeamRoleMember); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to add member: "+err.Error()))
		return
	}

	// Auto-create outbound link from lead to new member
	if m.linkStore != nil {
		leadAgent, err := m.agentStore.GetByID(ctx, team.LeadAgentID)
		if err == nil {
			m.autoCreateTeamLinks(ctx, teamID, leadAgent, []*store.AgentData{ag}, client.UserID())
		}
	}

	// Invalidate caches for all team members
	m.invalidateTeamCaches(ctx, teamID)

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{"ok": true}))
}

// --- Remove Member ---

type teamsRemoveMemberParams struct {
	TeamID  string `json:"teamId"`
	AgentID string `json:"agentId"` // agent UUID
}

func (m *TeamsMethods) handleRemoveMember(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsRemoveMemberParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}
	if params.TeamID == "" || params.AgentID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId and agentId are required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}
	agentID, err := uuid.Parse(params.AgentID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid agentId"))
		return
	}

	ctx := context.Background()

	// Validate team exists and prevent removing the lead
	team, err := m.teamStore.GetTeam(ctx, teamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "team not found: "+err.Error()))
		return
	}
	if agentID == team.LeadAgentID {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "cannot remove the team lead"))
		return
	}

	// Remove member
	if err := m.teamStore.RemoveMember(ctx, teamID, agentID); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to remove member: "+err.Error()))
		return
	}

	// Clean up team-specific links
	if m.linkStore != nil {
		if err := m.linkStore.DeleteTeamLinksForAgent(ctx, teamID, agentID); err != nil {
			slog.Warn("teams.members.remove: failed to clean up links", "error", err)
		}
	}

	// Invalidate caches for all remaining members + removed agent
	m.invalidateTeamCaches(ctx, teamID)
	if m.agentRouter != nil {
		ag, err := m.agentStore.GetByID(ctx, agentID)
		if err == nil {
			m.agentRouter.InvalidateAgent(ag.AgentKey)
		}
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{"ok": true}))
}

// invalidateTeamCaches invalidates agent caches for all members of a team
// and emits a pub/sub event for TeamToolManager cache invalidation.
func (m *TeamsMethods) invalidateTeamCaches(ctx context.Context, teamID uuid.UUID) {
	if m.agentRouter != nil {
		members, err := m.teamStore.ListMembers(ctx, teamID)
		if err == nil {
			for _, member := range members {
				if member.AgentKey != "" {
					m.agentRouter.InvalidateAgent(member.AgentKey)
				}
			}
		}
	}
	m.emitTeamCacheInvalidate()
}

// --- Update (settings) ---

type teamsUpdateParams struct {
	TeamID   string                 `json:"teamId"`
	Settings map[string]interface{} `json:"settings"`
}

func (m *TeamsMethods) handleUpdate(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsUpdateParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.TeamID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId is required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}

	ctx := context.Background()

	// Validate team exists
	if _, err := m.teamStore.GetTeam(ctx, teamID); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "team not found: "+err.Error()))
		return
	}

	// Validate settings against teamAccessSettings schema (strip unknown fields)
	type teamAccessSettings struct {
		AllowUserIDs  []string `json:"allow_user_ids"`
		DenyUserIDs   []string `json:"deny_user_ids"`
		AllowChannels []string `json:"allow_channels"`
		DenyChannels  []string `json:"deny_channels"`
	}
	raw, _ := json.Marshal(params.Settings)
	var access teamAccessSettings
	if err := json.Unmarshal(raw, &access); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid settings: "+err.Error()))
		return
	}
	cleaned, _ := json.Marshal(access)

	updates := map[string]any{"settings": json.RawMessage(cleaned)}
	if err := m.teamStore.UpdateTeam(ctx, teamID, updates); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "failed to update team: "+err.Error()))
		return
	}

	m.invalidateTeamCaches(ctx, teamID)

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{"ok": true}))
}

// --- Known Users ---

type teamsKnownUsersParams struct {
	TeamID string `json:"teamId"`
}

func (m *TeamsMethods) handleKnownUsers(_ context.Context, client *gateway.Client, req *protocol.RequestFrame) {
	if m.teamStore == nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, "teams not available (standalone mode)"))
		return
	}

	var params teamsKnownUsersParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid params"))
		return
	}

	if params.TeamID == "" {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "teamId is required"))
		return
	}

	teamID, err := uuid.Parse(params.TeamID)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInvalidRequest, "invalid teamId"))
		return
	}

	ctx := context.Background()
	users, err := m.teamStore.KnownUserIDs(ctx, teamID, 100)
	if err != nil {
		client.SendResponse(protocol.NewErrorResponse(req.ID, protocol.ErrInternal, err.Error()))
		return
	}

	client.SendResponse(protocol.NewOKResponse(req.ID, map[string]interface{}{
		"users": users,
	}))
}

// --- helpers ---

// autoCreateTeamLinks creates outbound agent_links from lead to each member.
// Only the lead can delegate to members — members cannot delegate back to lead
// or to other members. Silently skips existing links (UNIQUE constraint).
func (m *TeamsMethods) autoCreateTeamLinks(ctx context.Context, teamID uuid.UUID, leadAgent *store.AgentData, members []*store.AgentData, createdBy string) {
	for _, member := range members {
		if member.ID == leadAgent.ID {
			continue
		}
		link := &store.AgentLinkData{
			SourceAgentID: leadAgent.ID,
			TargetAgentID: member.ID,
			Direction:     store.LinkDirectionOutbound,
			TeamID:        &teamID,
			Description:   "auto-created by team",
			MaxConcurrent: 3,
			Status:        store.LinkStatusActive,
			CreatedBy:     createdBy,
		}
		if err := m.linkStore.CreateLink(ctx, link); err != nil {
			slog.Debug("teams: auto-link already exists or failed",
				"source", leadAgent.AgentKey, "target", member.AgentKey, "error", err)
		}
	}
}
