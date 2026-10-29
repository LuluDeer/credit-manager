package store

import (
	"context"
	"testing"
)

func TestInsertRejectedAttemptWritesZeroCostLedger(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{QuotaMicroUSD: 1_000_000})

	entry, err := st.InsertRejectedAttempt(ctx, RejectedAttempt{
		CallerID:    key.CallerID,
		PluginKeyID: key.ID,
		Model:       "gpt-test",
		Result:      "failed",
		Source:      "attempt_rejected",
		Summary:     "limit_rejected:quota",
	})
	if err != nil {
		t.Fatalf("InsertRejectedAttempt: %v", err)
	}
	if entry.CostMicroUSD != 0 || entry.Source != "attempt_rejected" {
		t.Fatalf("entry = %#v", entry)
	}
	if entry.Metrics.Result == nil || *entry.Metrics.Result != "failed" {
		t.Fatalf("result = %#v", entry.Metrics.Result)
	}

	gotKey, err := st.GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotKey.HeldAmountMicroUSD != 0 || gotKey.SettledSpendMicroUSD != 0 {
		t.Fatalf("quota mutated: held=%d settled=%d", gotKey.HeldAmountMicroUSD, gotKey.SettledSpendMicroUSD)
	}

	reservation, err := st.GetReservation(ctx, entry.ReservationID)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.Status != ReservationSettled || reservation.HeldMicroUSD != 0 || reservation.SettledMicroUSD == nil || *reservation.SettledMicroUSD != 0 {
		t.Fatalf("reservation = %#v", reservation)
	}

	entries, err := st.ListUsage(ctx, UsageFilter{PluginKeyID: key.ID, Limit: 10})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list usage: %v %#v", err, entries)
	}
}

func TestInsertRejectedAttemptRejectsCallerMismatch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{})
	_, err := st.InsertRejectedAttempt(ctx, RejectedAttempt{
		CallerID: "other-caller", PluginKeyID: key.ID, Model: "m",
	})
	if err == nil {
		t.Fatal("expected caller mismatch error")
	}
}
