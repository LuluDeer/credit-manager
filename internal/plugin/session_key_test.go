package plugin

import (
	"strings"
	"net/http"
	"testing"
)

func TestSessionKeyFromRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		body    string
		want    string
	}{
		{"claude header", http.Header{"X-Claude-Code-Session-Id": {"abc"}}, `{}`, "claude:abc"},
		{"codex header", http.Header{"Session_id": {"s1"}}, `{}`, "codex:s1"},
		{"prompt cache key", nil, `{"prompt_cache_key":"pk"}`, "pck:pk"},
		{"claude metadata", nil, `{"metadata":{"user_id":"user_x_account_y_session_z"}}`, "claude:user_x_account_y_session_z"},
		{"conversation", nil, `{"conversation":{"id":"cv"}}`, "conv:cv"},
		{"none", nil, `{"model":"x"}`, ""},
	}
	for _, c := range cases {
		if got := sessionKeyFromRequest(c.headers, []byte(c.body)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	a, af := sessionKeysFromRequest(nil, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	b, bf := sessionKeysFromRequest(nil, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`))
	if a == "" || af != "" || bf != a || b == a {
		t.Fatalf("turn1=(%q,%q) turn2=(%q,%q): turn2 fallback must equal turn1 primary", a, af, b, bf)
	}
	h := withSessionAffinityHeader(http.Header{"Session_id": {"s1"}}, nil)
	if h.Get(sessionAffinityHeader) == "" || h.Get(sessionAffinityHeader) == "codex:s1" {
		t.Fatalf("header should carry hashed key, got %q", h.Get(sessionAffinityHeader))
	}
}

// Golden values computed by CPA 8.0.36 extractMessageHashIDs (FNV-64a).
func TestOpeningMessageKeysMatchCPA(t *testing.T) {
	cases := []struct{ name, body, primary, fallback string }{
		{"openai short", `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`, "msg:708e3b2be6fac997", ""},
		{"openai full", `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"text","text":"he"},{"type":"text","text":"llo"}]},{"role":"assistant","content":"hey"},{"role":"user","content":"x"}]}`, "msg:4ce8bb4a463bb879", "msg:8423cb52dbb88903"},
		{"claude system array", `{"system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"u"}]}`, "msg:e19dc95105200b1a", ""},
		{"responses", `{"instructions":"I","input":[{"type":"reasoning"},{"role":"user","content":[{"type":"input_text","text":"q"}]},{"type":"function_call"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}]}`, "msg:0bf2fed6bf140cc1", "msg:675f2c8fc08d5ee8"},
		{"gemini", `{"systemInstruction":{"parts":[{"text":"G"}]},"contents":[{"role":"user","parts":[{"text":"u1"},{"text":"u2"}]},{"role":"model","parts":[{"text":"m"}]}]}`, "msg:ad975241cb24c09a", "msg:eaa3b16a43215d73"},
		{"no user", `{"messages":[{"role":"system","content":"s"}]}`, "", ""},
	}
	for _, c := range cases {
		p, f := sessionKeysFromRequest(nil, []byte(c.body))
		if p != c.primary || f != c.fallback {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, p, f, c.primary, c.fallback)
		}
	}
	long := strings.Repeat("x", 150)
	p, _ := sessionKeysFromRequest(nil, []byte(`{"messages":[{"role":"user","content":"`+long+`"}]}`))
	if p != cpaSessionHash("", long[:100], "") {
		t.Fatalf("truncation mismatch")
	}
	h := withSessionAffinityHeader(nil, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`))
	if v := h.Get(sessionAffinityHeader); !strings.Contains(v, ",") {
		t.Fatalf("header should carry primary,fallback: %q", v)
	}
}
