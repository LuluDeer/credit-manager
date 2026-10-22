package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestForwardHeadersSetsReservationAndStripsClientControl(t *testing.T) {
	in := http.Header{}
	in.Set(reservationHeader, "forged")
	in.Set(sessionAffinityHeader, "forged")
	in.Set("Authorization", "Bearer x")
	out := forwardHeaders(in, nil, "res-42")
	if got := out.Get(reservationHeader); got != "res-42" {
		t.Fatalf("reservation header = %q", got)
	}
	if out.Get(sessionAffinityHeader) == "forged" {
		t.Fatal("client session header must be stripped")
	}
	if in.Get(reservationHeader) != "forged" {
		t.Fatal("input headers must not be mutated")
	}
	if out2 := forwardHeaders(in, nil, ""); out2.Get(reservationHeader) != "" {
		t.Fatal("forged reservation must not pass through")
	}
}

func TestQuiesceRPCIsIdempotent(t *testing.T) {
	for i := 0; i < 2; i++ {
		resp, err := HandleMethod(pluginabi.MethodPluginQuiesce, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(resp, &env); err != nil || !env.OK {
			t.Fatalf("quiesce #%d: %s", i, resp)
		}
	}
}

func TestRegistrationDeclaresSchedulerAcrossPriorities(t *testing.T) {
	raw, err := json.Marshal(pluginRegistration(6))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"scheduler_across_priorities":true`) {
		t.Fatalf("missing capability: %s", raw)
	}
}
