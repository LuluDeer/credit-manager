package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/yuluo688/credit-manager/internal/money"
)

// RejectedAttempt is a zero-cost usage row for a request that never held quota
// (auth/plan/reserve/scheduler rejection before a normal Reserve succeeded).
type RejectedAttempt struct {
	CallerID    string
	PluginKeyID string
	Model       string
	Result      string
	Source      string
	Summary     string
}

// InsertRejectedAttempt writes a settled reservation (held=0) and a matching
// usage_ledger row without mutating key holds or spend. Ledger requires a
// reservation_id FK, so the reservation exists only for attribution.
func (s *Store) InsertRejectedAttempt(ctx context.Context, attempt RejectedAttempt) (UsageEntry, error) {
	attempt.CallerID = strings.TrimSpace(attempt.CallerID)
	attempt.PluginKeyID = strings.TrimSpace(attempt.PluginKeyID)
	attempt.Model = strings.TrimSpace(attempt.Model)
	attempt.Result = strings.TrimSpace(attempt.Result)
	attempt.Source = strings.TrimSpace(attempt.Source)
	attempt.Summary = strings.TrimSpace(attempt.Summary)
	if attempt.CallerID == "" || attempt.PluginKeyID == "" || attempt.Model == "" {
		return UsageEntry{}, fmt.Errorf("%w: caller, plugin key, and model are required", ErrInvalidArgument)
	}
	if attempt.Result == "" {
		attempt.Result = "failed"
	}
	if attempt.Source == "" {
		attempt.Source = "attempt_rejected"
	}
	if attempt.Summary == "" {
		attempt.Summary = attempt.Source
	}

	reservationID := newID()
	ledgerID := newID()
	idempotency := "reject:" + reservationID
	now := nowUnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageEntry{}, err
	}
	defer tx.Rollback()

	// Verify key ownership for FK integrity without enforcing allowlists/quotas:
	// this path records a rejection that already happened under those checks.
	var keyCaller string
	if err := tx.QueryRowContext(ctx, `SELECT caller_id FROM plugin_keys WHERE id = ?`, attempt.PluginKeyID).Scan(&keyCaller); err != nil {
		return UsageEntry{}, fmt.Errorf("%w: plugin key not found", ErrPluginKeyNotFound)
	}
	if keyCaller != attempt.CallerID {
		return UsageEntry{}, fmt.Errorf("%w: plugin key caller mismatch", ErrInvalidArgument)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO reservations(
		id, caller_id, plugin_key_id, idempotency_key, model, request_token_estimate, held_micro_usd,
		settled_micro_usd, status, request_summary, settlement_summary,
		created_at_unix_ms, updated_at_unix_ms, settled_at_unix_ms, execution_finished_at_unix_ms
	) VALUES (?, ?, ?, ?, ?, 1, 0, 0, 'settled', ?, ?, ?, ?, ?, ?)`,
		reservationID, attempt.CallerID, attempt.PluginKeyID, idempotency, attempt.Model,
		attempt.Summary, attempt.Summary, now, now, now, now)
	if err != nil {
		return UsageEntry{}, fmt.Errorf("create rejected reservation: %w", err)
	}

	resultLabel := attempt.Result
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_ledger(
		id, reservation_id, caller_id, plugin_key_id, executor_type, model, upstream_response_model, pricing_rule_id,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
		total_tokens, cost_micro_usd, estimated_cost_micro_usd, source,
		tier, result, first_token_latency_ms, generation_duration_ms, tokens_per_second, thinking_intensity,
		auth_id, auth_index, auth_name, auth_label, auth_provider, auth_type, auth_email, auth_path, created_at_unix_ms
	) VALUES (?, ?, ?, ?, NULL, ?, NULL, NULL, 0, 0, 0, 0, 0, 0, 0, 0, 0, ?, NULL, ?, NULL, NULL, NULL, NULL,
		NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, ?)`,
		ledgerID, reservationID, attempt.CallerID, attempt.PluginKeyID, attempt.Model,
		attempt.Source, resultLabel, now)
	if err != nil {
		return UsageEntry{}, fmt.Errorf("write rejected usage ledger: %w", err)
	}

	details := fmt.Sprintf(`{"source":%q,"result":%q}`, attempt.Source, attempt.Result)
	if err := insertAudit(ctx, tx, attempt.CallerID, attempt.PluginKeyID, reservationID, "attempt_rejected", money.MicroUSD(0), details, now); err != nil {
		return UsageEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return UsageEntry{}, err
	}
	return s.GetUsage(ctx, ledgerID)
}
