package management

import (
	"context"
	"net/http"
	"testing"

	"github.com/yuluo688/credit-manager/internal/config"
	"github.com/yuluo688/credit-manager/internal/service"
)

func TestUpdateAuthRoutingSettingsPartialAndValidation(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CREDIT_MANAGER_TEST_PEPPERS", "active:0123456789abcdef0123456789abcdef")
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Keys.PepperEnv = "CREDIT_MANAGER_TEST_PEPPERS"
	cfg.Keys.ActivePepperID = "active"
	svc, err := service.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if resp, _ := updateAuthRoutingSettings(ctx, svc, []byte(`{"strategy":"round-robin","session_affinity":true,"session_affinity_ttl_seconds":1800}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("full update = %d %s", resp.StatusCode, resp.Body)
	}
	if resp, _ := updateAuthRoutingSettings(ctx, svc, []byte(`{"strategy":"least-busy"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("partial update = %d %s", resp.StatusCode, resp.Body)
	}
	got, _ := svc.AuthRoutingSettings(ctx)
	if got.Strategy != "least-busy" || !got.SessionAffinity || got.SessionAffinityTTLSeconds != 1800 {
		t.Fatalf("partial update clobbered fields: %+v", got)
	}
	for _, body := range []string{`{"strategy":"weighted"}`, `{"session_affinity_ttl_seconds":5}`, `{"session_affinity_ttl_seconds":-1}`, `nope`} {
		if resp, _ := updateAuthRoutingSettings(ctx, svc, []byte(body)); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s => %d, want 400", body, resp.StatusCode)
		}
	}
	if after, _ := svc.AuthRoutingSettings(ctx); after.Strategy != "least-busy" || after.SessionAffinityTTLSeconds != 1800 {
		t.Fatalf("rejected update changed settings: %+v", after)
	}
}
