package repo

import (
	"context"

	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
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
type Dashboard struct{ gdb *gorm.DB }

// NewDashboard wires the repo to the DB.
func NewDashboard(gdb *gorm.DB) *Dashboard { return &Dashboard{gdb: gdb} }

// ForUser returns counts scoped to userID. Each count is a single
// query to keep GORM's type mapping simple; the whole function is
// well under 10 ms in practice.
func (d *Dashboard) ForUser(ctx context.Context, userID int32) (DashboardStats, error) {
	var s DashboardStats

	if err := d.gdb.WithContext(ctx).Model(&models.Chat{}).
		Where("user_id = ?", userID).Count(&s.TotalChats).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.Document{}).
		Where("user_id = ?", userID).Count(&s.TotalDocuments).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.WritingSession{}).
		Joins("JOIN chats c ON c.id = writing_sessions.chat_id").
		Where("c.user_id = ?", userID).
		Count(&s.TotalWritingSessions).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.Mission{}).
		Joins("JOIN chats c ON c.id = missions.chat_id").
		Where("c.user_id = ?", userID).
		Count(&s.TotalMissions).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.Chat{}).
		Where("user_id = ? AND COALESCE(chat_type, 'research') = 'research'", userID).
		Count(&s.ResearchSessions).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.Chat{}).
		Where("user_id = ? AND chat_type = ?", userID, "writing").
		Count(&s.WritingSessions).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.Mission{}).
		Joins("JOIN chats c ON c.id = missions.chat_id").
		Where("c.user_id = ? AND missions.status = ?", userID, "completed").
		Count(&s.CompletedMissions).Error; err != nil {
		return DashboardStats{}, err
	}
	if err := d.gdb.WithContext(ctx).Model(&models.Mission{}).
		Joins("JOIN chats c ON c.id = missions.chat_id").
		Where("c.user_id = ? AND missions.status IN ?", userID, []string{"pending", "running"}).
		Count(&s.ActiveMissions).Error; err != nil {
		return DashboardStats{}, err
	}

	s.RecentActivity = []any{}
	return s, nil
}
