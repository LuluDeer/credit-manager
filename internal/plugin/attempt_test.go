package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/yuluo688/credit-manager/internal/money"
	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestImageFailureSettlesZeroCostIntoLedger(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	key, _, err := svc.MintKey(ctx, service.BootstrapCallerID, "image-fail", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := svc.Store().Reserve(ctx, store.ReserveRequest{
		CallerID: key.CallerID, PluginKeyID: key.ID, IdempotencyKey: "image-fail",
		Model: "image-model", RequestTokenEstimate: 1, AmountMicroUSD: money.MicroUSD(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "image-fail"
	imageHoldsMu.Lock()
	imageHolds[requestID] = imageHold{
		reservation: reservation,
		plan: service.ReservePlan{
			Model: "image-model", ImageCount: 2,
			Price: money.PricePerMTok{BillingMode: money.BillingPerImage, PerImage: 5_000_000},
		},
	}
	imageHoldsMu.Unlock()
	t.Cleanup(func() {
		imageHoldsMu.Lock()
		delete(imageHolds, requestID)
		imageHoldsMu.Unlock()
	})

	raw, err := json.Marshal(pluginapi.RequestCompletion{
		RequestID: requestID, Outcome: pluginapi.RequestCompletionFailed, StatusCode: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeInterceptedRequest(raw); err != nil {
		t.Fatal(err)
	}

	gotKey, err := svc.Store().GetPluginKey(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotKey.HeldAmountMicroUSD != 0 || gotKey.SettledSpendMicroUSD != 0 {
		t.Fatalf("quota: held=%d settled=%d (must not charge images on failure)", gotKey.HeldAmountMicroUSD, gotKey.SettledSpendMicroUSD)
	}
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list usage: %v %#v", err, entries)
	}
	if entries[0].CostMicroUSD != 0 || entries[0].Source != service.SourceAttemptFailed {
		t.Fatalf("entry = %#v", entries[0])
	}
	final, err := svc.Store().GetReservation(ctx, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.ReservationSettled {
		t.Fatalf("status = %s, want settled", final.Status)
	}
}

func TestPreUpstreamCancelSettlesCancelledIntoLedger(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "lifecycle", MatchKind: store.MatchGlob, Pattern: "*", Priority: 10, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, material, err := svc.MintKey(ctx, service.BootstrapCallerID, "cancel-ledger", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "cancel-into-ledger"
	trackStreamLifecycle(requestID, svc)
	completeStreamLifecycle(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionCanceled})
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		return nil, 0, fmt.Errorf("host must not start: %s", method)
	}
	body := []byte(`{"model":"cancel-ledger","stream":true,"max_tokens":1,"messages":[{"role":"user","content":"test"}]}`)
	err = runStream(ctx, svc, rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "cancel-ledger", SourceFormat: "openai", Format: "openai", Stream: true,
		Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}}, Payload: body,
	}}, "downstream", requestID)
	if !errors.Is(err, errClientDisconnectedBeforeUpstream) {
		t.Fatalf("runStream error = %v", err)
	}
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list usage: %v %#v", err, entries)
	}
	if entries[0].CostMicroUSD != 0 || entries[0].Source != service.SourceAttemptCancelled {
		t.Fatalf("entry = %#v", entries[0])
	}
	if entries[0].Metrics.Result == nil || *entries[0].Metrics.Result != "cancelled" {
		t.Fatalf("result = %#v", entries[0].Metrics.Result)
	}
}

func TestExecuteUpstreamErrorSettlesZeroCost(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 10, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, material, err := svc.MintKey(ctx, service.BootstrapCallerID, "upstream-err", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldHost := HostCall
	defer func() { HostCall = oldHost }()
	HostCall = func(method string, raw []byte) ([]byte, int, error) {
		return nil, 0, fmt.Errorf("upstream boom")
	}
	body := []byte(`{"model":"upstream-err","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	raw, err := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "upstream-err", SourceFormat: "openai", Format: "openai",
		Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}}, Payload: body,
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := execute(raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(resp, &envelope); err != nil {
		t.Fatal(err)
	}
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list usage: %v %#v envelope=%v", err, entries, envelope)
	}
	if entries[0].CostMicroUSD != 0 || entries[0].Source != service.SourceAttemptFailed {
		t.Fatalf("entry = %#v", entries[0])
	}
}

func TestReserveRejectedRecordsLedger(t *testing.T) {
	ctx := context.Background()
	svc := newLifecycleTestService(t)
	if err := svc.Store().PutPricingRule(ctx, store.PricingRule{
		ID: "all", MatchKind: store.MatchGlob, Pattern: "*", Priority: 10, Enabled: true,
		Price: money.PricePerMTok{Input: 1_000_000, Output: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	key, material, err := svc.MintKeyWithPolicy(ctx, service.MintKeyRequest{
		CallerID: service.BootstrapCallerID, Label: "allowlist", QuotaMicroUSD: 10_000_000,
		AllowedModels: []string{"only-this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"not-allowed","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	raw, err := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model: "not-allowed", SourceFormat: "openai", Format: "openai",
		Headers: http.Header{"Authorization": []string{"Bearer " + material.Plaintext}}, Payload: body,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execute(raw); err != nil {
		t.Fatal(err)
	}
	entries, err := svc.Store().ListUsage(ctx, store.UsageFilter{PluginKeyID: key.ID, Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("list usage: %v %#v", err, entries)
	}
	if entries[0].Source != service.SourceAttemptRejected || entries[0].CostMicroUSD != 0 {
		t.Fatalf("entry = %#v", entries[0])
	}
}
