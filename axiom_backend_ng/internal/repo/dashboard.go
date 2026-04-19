package repo

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DashboardStats matches axiom_backend/api/schemas.py:DashboardStats.
type DashboardStats struct {
	TotalChats           int64 `json:"total_chats"`
	TotalDocuments       int64 `json:"total_documents"`
	TotalWritingSessions int64 `json:"total_writing_sessions"`
	TotalMissions        int64 `json:"total_missions"`
	ResearchSessions     int64 `json:"research_sessions"`
	WritingSessions      int64 `json:"writing_sessions"`
	CompletedMissions    int64 `json:"completed_missions"`
	ActiveMissions       int64 `json:"active_missions"`
	RecentActivity       []any `json:"recent_activity"`
}

// Dashboard computes aggregate stats for a single user.
type Dashboard struct{ pool *pgxpool.Pool }

// NewDashboard wires the repo to the pool.
func NewDashboard(pool *pgxpool.Pool) *Dashboard { return &Dashboard{pool: pool} }

// ForUser returns counts scoped to userID. RecentActivity is left as
// an empty slice in this iteration; the Python endpoint also returns
// an empty list by default.
func (d *Dashboard) ForUser(ctx context.Context, userID int32) (DashboardStats, error) {
	const q = `
		SELECT
			(SELECT COUNT(*) FROM chats WHERE user_id = $1) AS total_chats,
			(SELECT COUNT(*) FROM documents WHERE user_id = $1) AS total_documents,
			(SELECT COUNT(*) FROM writing_sessions ws JOIN chats c ON c.id = ws.chat_id WHERE c.user_id = $1) AS total_writing_sessions,
			(SELECT COUNT(*) FROM missions m JOIN chats c ON c.id = m.chat_id WHERE c.user_id = $1) AS total_missions,
			(SELECT COUNT(*) FROM chats WHERE user_id = $1 AND COALESCE(chat_type, 'research') = 'research') AS research_sessions,
			(SELECT COUNT(*) FROM chats WHERE user_id = $1 AND chat_type = 'writing') AS writing_sessions,
			(SELECT COUNT(*) FROM missions m JOIN chats c ON c.id = m.chat_id WHERE c.user_id = $1 AND m.status = 'completed') AS completed_missions,
			(SELECT COUNT(*) FROM missions m JOIN chats c ON c.id = m.chat_id WHERE c.user_id = $1 AND m.status IN ('pending','running')) AS active_missions
	`
	var s DashboardStats
	err := d.pool.QueryRow(ctx, q, userID).Scan(
		&s.TotalChats, &s.TotalDocuments, &s.TotalWritingSessions, &s.TotalMissions,
		&s.ResearchSessions, &s.WritingSessions, &s.CompletedMissions, &s.ActiveMissions,
	)
	if err != nil {
		return DashboardStats{}, err
	}
	s.RecentActivity = []any{}
	return s, nil
}
