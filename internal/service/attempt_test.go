package service

import (
	"context"
	"testing"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestSettleZeroCostAttemptRecordsFailedUsage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("CREDIT_MANAGER_TEST_PEPPERS", "active:0123456789abcdef0123456789abcdef")
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Keys.PepperEnv = "CREDIT_MANAGER_TEST_PEPPERS"
	cfg.Keys.ActivePepperID = "active"
	cfg.Settlement.MissingUsage = config.MissingUsageRelease // must still settle despite release policy
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 1, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.MintKey(ctx, BootstrapCallerID, "zero-cost", 10_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.BuildReservePlan(ctx, "gpt-test", []byte(`{"model":"gpt-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Reserve(ctx, key, plan, "zero-fail")
	if err != nil {
		t.Fatal(err)
	}
	heldBefore := reservation.HeldMicroUSD
	if heldBefore <= 0 {
		t.Fatalf("held = %d", heldBefore)
	}

	if err := svc.SettleZeroCostAttempt(ctx, reservation, plan.Model, AttemptMetrics("failed"), SourceAttemptFailed, "upstream_error:boom"); err != nil {
		t.Fatalf("SettleZeroCostAttempt: %v", err)
	}

	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %#v", err, entries)
	}
	if entries[0].CostMicroUSD != 0 || entries[0].Source != SourceAttemptFailed {
		t.Fatalf("entry = %#v", entries[0])
	}
	if entries[0].Metrics.Result == nil || *entries[0].Metrics.Result != "failed" {
		t.Fatalf("result = %#v", entries[0].Metrics.Result)
	}
	gotKey, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotKey.HeldAmountMicroUSD != 0 || gotKey.SettledSpendMicroUSD != 0 {
		t.Fatalf("quota: held=%d settled=%d", gotKey.HeldAmountMicroUSD, gotKey.SettledSpendMicroUSD)
	}
	final, err := svc.Store().GetReservation(ctx, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.ReservationSettled {
		t.Fatalf("status = %s", final.Status)
	}
}

func TestRecordRejectedAttemptNoQuotaHold(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("CREDIT_MANAGER_TEST_PEPPERS", "active:0123456789abcdef0123456789abcdef")
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Keys.PepperEnv = "CREDIT_MANAGER_TEST_PEPPERS"
	cfg.Keys.ActivePepperID = "active"
	svc, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	key, _, err := svc.MintKey(ctx, BootstrapCallerID, "reject", 10_000_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordRejectedAttempt(ctx, key, "gpt-test", "failed", SourceAttemptRejected, "model_disabled"); err != nil {
		t.Fatal(err)
	}
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %#v", err, entries)
	}
	if entries[0].CostMicroUSD != 0 || entries[0].Source != SourceAttemptRejected {
		t.Fatalf("entry = %#v", entries[0])
	}
}
