package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
)

// sessionAffinityHeader carries the plugin-derived session key from the
// executor (which sees the body) to scheduler.pick (which only sees headers).
// The value is a hash, never the raw client identifier.
const sessionAffinityHeader = "X-Credit-Manager-Session"

// sessionKeyFromRequest mirrors the explicit signals CPA's session affinity
// honours (sdk/cliproxy/auth selector.go extractSessionIDs), in the same order,
// plus CPA's last-resort stable hash of the opening messages.
func sessionKeyFromRequest(headers http.Header, body []byte) string {
	primary, _ := sessionKeysFromRequest(headers, body)
	return primary
}

// sessionKeysFromRequest returns the primary session key and, for the
// message-hash fallback only, CPA's secondary (short) key.
func sessionKeysFromRequest(headers http.Header, body []byte) (string, string) {
	for _, h := range []struct{ name, prefix string }{
		{"X-Claude-Code-Session-Id", "claude:"},
		{"Session-Id", "codex:"},
		{"Session_id", "codex:"},
		{"X-Session-ID", "header:"},
		{"X-Session-Affinity", "affinity:"},
		{"X-Client-Request-Id", "clientreq:"},
	} {
		if v := headerValueFold(headers, h.name); v != "" {
			return h.prefix + v, ""
		}
	}
	if len(body) == 0 {
		return "", ""
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return "", ""
	}
	str := func(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }
	meta, _ := doc["metadata"].(map[string]any)
	if meta != nil {
		if uid := str(meta["user_id"]); strings.Contains(uid, "session") {
			return "claude:" + uid, ""
		}
	}
	for _, k := range []string{"session_id", "sessionId"} {
		if v := str(doc[k]); v != "" {
			return "session:" + v, ""
		}
	}
	if v := str(doc["prompt_cache_key"]); v != "" {
		return "pck:" + v, ""
	}
	if conv, ok := doc["conversation"].(map[string]any); ok {
		if v := str(conv["id"]); v != "" {
			return "conv:" + v, ""
		}
	} else if v := str(doc["conversation"]); v != "" {
		return "conv:" + v, ""
	}
	if meta != nil {
		if v := str(meta["user_id"]); v != "" {
			return "user:" + v, ""
		}
	}
	if v := str(doc["conversation_id"]); v != "" {
		return "conv:" + v, ""
	}
	return openingMessageKeys(doc)
}

func headerValueFold(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if v := strings.TrimSpace(headers.Get(name)); v != "" {
		return v
	}
	for k, vals := range headers {
		if strings.EqualFold(k, name) {
			for _, v := range vals {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// openingMessageKeys ports CPA's extractMessageHashIDs (sdk/cliproxy/auth
// selector.go): system prompt + first user message + first assistant reply,
// each truncated to 100 bytes, hashed with FNV-64a. With an assistant reply the
// primary key is the full hash and the fallback the short (system+user) hash,
// so turn 1 (short) and turn 2+ (full -> fallback short) stay on one auth.
func openingMessageKeys(doc map[string]any) (primary, fallback string) {
	var system, user, assistant string
	set := func(dst *string, text string) {
		if *dst == "" {
			*dst = truncate(text, 100)
		}
	}
	if messages, ok := doc["messages"].([]any); ok {
		for _, raw := range messages {
			msg, _ := raw.(map[string]any)
			role, _ := msg["role"].(string)
			content := cpaMessageContent(msg["content"])
			if content == "" {
				continue
			}
			switch role {
			case "system":
				set(&system, content)
			case "user":
				set(&user, content)
			case "assistant":
				set(&assistant, content)
			}
			if system != "" && user != "" && assistant != "" {
				break
			}
		}
	}
	if system == "" {
		switch top := doc["system"].(type) {
		case []any:
			for _, raw := range top {
				part, _ := raw.(map[string]any)
				if text, _ := part["text"].(string); text != "" {
					system = truncate(text, 100)
					break
				}
			}
		case string:
			system = truncate(top, 100)
		}
	}
	if system == "" && user == "" {
		if si, ok := doc["systemInstruction"].(map[string]any); ok {
			if parts, ok := si["parts"].([]any); ok {
				for _, raw := range parts {
					part, _ := raw.(map[string]any)
					if text, _ := part["text"].(string); text != "" {
						system = truncate(text, 100)
						break
					}
				}
			}
		}
		if contents, ok := doc["contents"].([]any); ok {
			for _, raw := range contents {
				msg, _ := raw.(map[string]any)
				role, _ := msg["role"].(string)
				parts, _ := msg["parts"].([]any)
				for _, rawPart := range parts {
					part, _ := rawPart.(map[string]any)
					text, _ := part["text"].(string)
					if text == "" {
						continue
					}
					switch role {
					case "user":
						set(&user, text)
					case "model":
						set(&assistant, text)
					}
					break
				}
				if user != "" && assistant != "" {
					break
				}
			}
		}
	}
	if system == "" && user == "" {
		if instr, _ := doc["instructions"].(string); instr != "" {
			system = truncate(instr, 100)
		}
		if input, ok := doc["input"].([]any); ok {
			for _, raw := range input {
				item, _ := raw.(map[string]any)
				itemType, _ := item["type"].(string)
				if itemType == "reasoning" || (itemType != "" && itemType != "message") {
					continue
				}
				role, _ := item["role"].(string)
				if itemType == "" && role == "" {
					continue
				}
				var text string
				if c, ok := item["content"].(string); ok {
					text = c
				} else {
					text = cpaResponsesContent(item["content"])
				}
				if text == "" {
					continue
				}
				switch role {
				case "developer", "system":
					set(&system, text)
				case "user":
					set(&user, text)
				case "assistant":
					set(&assistant, text)
				}
				if user != "" && assistant != "" {
					break
				}
			}
		}
	}
	if user == "" {
		return "", ""
	}
	short := cpaSessionHash(system, user, "")
	if assistant == "" {
		return short, ""
	}
	return cpaSessionHash(system, user, assistant), short
}

func cpaSessionHash(system, user, assistant string) string {
	h := fnv.New64a()
	if system != "" {
		h.Write([]byte("sys:" + system + "\n"))
	}
	if user != "" {
		h.Write([]byte("usr:" + user + "\n"))
	}
	if assistant != "" {
		h.Write([]byte("ast:" + assistant + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

// cpaMessageContent mirrors CPA's extractMessageContent.
func cpaMessageContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var texts []string
		for _, raw := range c {
			part, _ := raw.(map[string]any)
			if typ, _ := part["type"].(string); typ == "text" {
				if text, _ := part["text"].(string); text != "" {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, " ")
	}
	return ""
}

// cpaResponsesContent mirrors CPA's extractResponsesAPIContent.
func cpaResponsesContent(content any) string {
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		switch typ, _ := part["type"].(string); typ {
		case "input_text", "output_text", "text":
			if text, _ := part["text"].(string); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, " ")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// hashedSessionKey returns the header value for a session key.
func hashedSessionKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// reservationHeader carries the reservation ID of a forwarded request so
// scheduler.pick can attribute the chosen auth to exactly that request.
// CPA executors build upstream headers from allowlists, so X-Credit-Manager-*
// headers are not forwarded upstream.
const reservationHeader = "X-Credit-Manager-Reservation"

// forwardHeaders returns a copy of client headers for the nested host call,
// carrying the plugin's session key and reservation ID.
func forwardHeaders(headers http.Header, body []byte, reservationID string) http.Header {
	out := withSessionAffinityHeader(headers, body)
	out.Del(reservationHeader)
	if reservationID = strings.TrimSpace(reservationID); reservationID != "" {
		out.Set(reservationHeader, reservationID)
	}
	return out
}

// withSessionAffinityHeader returns a copy of headers carrying the session key.
func withSessionAffinityHeader(headers http.Header, body []byte) http.Header {
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	// Never trust client-supplied plugin control headers.
	out.Del(sessionAffinityHeader)
	out.Del(reservationHeader)
	primary, fallback := sessionKeysFromRequest(headers, body)
	if v := hashedSessionKey(primary); v != "" {
		if f := hashedSessionKey(fallback); f != "" {
			v += "," + f
		}
		out.Set(sessionAffinityHeader, v)
	}
	return out
}
