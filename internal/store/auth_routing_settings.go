package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Plugin auth pick strategies. They only apply when the plugin itself picks an
// auth, i.e. when at least one candidate has a concurrency limit.
const (
	AuthRoutingRoundRobin = "round-robin"
	AuthRoutingFillFirst  = "fill-first"
	AuthRoutingLeastBusy  = "least-busy"

	DefaultSessionAffinityTTLSeconds int64 = 3600
)

// AuthRoutingSettings controls how the plugin picks among non-full candidates.
type AuthRoutingSettings struct {
	Strategy string `json:"strategy"`
	// SessionAffinity keeps one client session on one auth (prompt cache reuse).
	SessionAffinity bool `json:"session_affinity"`
	// SessionAffinityTTLSeconds is how long an idle binding is kept (default 3600).
	SessionAffinityTTLSeconds int64      `json:"session_affinity_ttl_seconds"`
	UpdatedAt                 *time.Time `json:"updated_at,omitempty"`
}

// NormalizeAuthRoutingStrategy returns a known strategy or "" when invalid.
// Empty input maps to the default round-robin.
func NormalizeAuthRoutingStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", AuthRoutingRoundRobin, "roundrobin", "round_robin":
		return AuthRoutingRoundRobin
	case AuthRoutingFillFirst, "fillfirst", "fill_first":
		return AuthRoutingFillFirst
	case AuthRoutingLeastBusy, "leastbusy", "least_busy":
		return AuthRoutingLeastBusy
	default:
		return ""
	}
}

func (s *Store) GetAuthRoutingSettings(ctx context.Context) (AuthRoutingSettings, error) {
	settings := AuthRoutingSettings{Strategy: AuthRoutingRoundRobin, SessionAffinityTTLSeconds: DefaultSessionAffinityTTLSeconds}
	var strategy string
	var affinity, ttl, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT strategy, session_affinity, session_affinity_ttl_seconds, updated_at_unix_ms FROM auth_routing_settings WHERE id = 1`).Scan(&strategy, &affinity, &ttl, &updated)
	if err == sql.ErrNoRows {
		return settings, nil
	}
	if err != nil {
		return AuthRoutingSettings{}, fmt.Errorf("get auth routing settings: %w", err)
	}
	if normalized := NormalizeAuthRoutingStrategy(strategy); normalized != "" {
		settings.Strategy = normalized
	}
	settings.SessionAffinity = affinity != 0
	if ttl > 0 {
		settings.SessionAffinityTTLSeconds = ttl
	}
	value := time.UnixMilli(updated).UTC()
	settings.UpdatedAt = &value
	return settings, nil
}

func (s *Store) UpsertAuthRoutingSettings(ctx context.Context, settings AuthRoutingSettings) (AuthRoutingSettings, error) {
	strategy := NormalizeAuthRoutingStrategy(settings.Strategy)
	if strategy == "" {
		return AuthRoutingSettings{}, fmt.Errorf("%w: unknown routing strategy %q", ErrInvalidArgument, settings.Strategy)
	}
	ttl := settings.SessionAffinityTTLSeconds
	if ttl == 0 {
		ttl = DefaultSessionAffinityTTLSeconds
	}
	if ttl < 60 || ttl > 7*24*3600 {
		return AuthRoutingSettings{}, fmt.Errorf("%w: session affinity ttl must be between 60 and 604800 seconds", ErrInvalidArgument)
	}
	affinity := int64(0)
	if settings.SessionAffinity {
		affinity = 1
	}
	now := nowUnixMilli()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_routing_settings(id, strategy, session_affinity, session_affinity_ttl_seconds, updated_at_unix_ms) VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET strategy = excluded.strategy, session_affinity = excluded.session_affinity,
			session_affinity_ttl_seconds = excluded.session_affinity_ttl_seconds, updated_at_unix_ms = excluded.updated_at_unix_ms`, strategy, affinity, ttl, now); err != nil {
		return AuthRoutingSettings{}, fmt.Errorf("save auth routing settings: %w", err)
	}
	updated := time.UnixMilli(now).UTC()
	return AuthRoutingSettings{Strategy: strategy, SessionAffinity: settings.SessionAffinity, SessionAffinityTTLSeconds: ttl, UpdatedAt: &updated}, nil
}
