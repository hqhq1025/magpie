package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/claudeseal"
	"github.com/yetone/magpie/internal/provider"
)

// A Codex turn on a Claude model that searched with magpie's search keeps
// the thinking from both sides of the search, but not the search's own
// tool call and result. Replayed together, the two blocks form one
// message Claude never wrote, and Anthropic answers 400 "thinking blocks
// in the latest assistant message cannot be modified" (NVIDIA Inference,
// 2026-10-28). Only the thinking written after the search goes back.
func TestClaudeThinkingAroundSearchReplays(t *testing.T) {
	seal := func(thinking, sig string) string {
		b, _ := json.Marshal(map[string]string{"type": "thinking", "thinking": thinking, "signature": sig})
		return claudeseal.Seal(b)
	}
	input, _ := json.Marshal([]any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Look it up."}}},
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": seal("search first", "sigBefore")},
		map[string]any{"type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": "q"}},
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": seal("now answer", "sigAfter")},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Found it."}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Thanks."}}},
	})
	req, err := parseResponses([]byte(`{"model":"m","reasoning":{"effort":"high"},"input":` + string(input) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := build(provider.Anthropic, req, "claude-opus-5-5", "api.anthropic.com", false)
	if err != nil {
		t.Fatal(err)
	}
	var sigs []string
	for _, m := range gjson.GetBytes(body, "messages").Array() {
		if m.Get("role").String() != "assistant" {
			continue
		}
		for _, c := range m.Get("content").Array() {
			if c.Get("type").String() == "thinking" {
				sigs = append(sigs, c.Get("signature").String())
			}
		}
	}
	if strings.Join(sigs, ",") != "sigAfter" {
		t.Fatalf("replayed thinking signatures %v, want only sigAfter: %s", sigs, gjson.GetBytes(body, "messages"))
	}
}

// A search whose call Codex didn't keep (it failed, so no web_search_call
// item) leaves Claude's two blocks side by side before the tool call. Sent
// together they were refused as modified thinking; the later one alone goes.
// Blocks with the assistant's words between them are both kept.
func TestClaudeThinkingSideBySideReplaysTheLater(t *testing.T) {
	seal := func(thinking, sig string) map[string]any {
		b, _ := json.Marshal(map[string]string{"type": "thinking", "thinking": thinking, "signature": sig})
		return map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": claudeseal.Seal(b)}
	}
	user := func(s string) map[string]any {
		return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": s}}}
	}
	said := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Checking."}}}
	call := map[string]any{"type": "function_call", "call_id": "c1", "name": "exec_command", "arguments": "{}"}
	out := map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"}
	for _, c := range []struct {
		name  string
		input []any
		want  string
	}{
		{"side by side", []any{user("Price?"), seal("search first", "sigBefore"), seal("search failed", "sigAfter"), call, out}, "sigAfter"},
		{"words between", []any{user("Price?"), seal("plan", "sig1"), said, seal("act", "sig2"), call, out}, "sig1,sig2"},
	} {
		input, _ := json.Marshal(c.input)
		req, err := parseResponses([]byte(`{"model":"m","reasoning":{"effort":"high"},"input":` + string(input) + `}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := build(provider.Anthropic, req, "claude-opus-5-5", "api.anthropic.com", false)
		if err != nil {
			t.Fatal(err)
		}
		var sigs []string
		for _, m := range gjson.GetBytes(body, "messages").Array() {
			for _, part := range m.Get("content").Array() {
				if part.Get("type").String() == "thinking" {
					sigs = append(sigs, part.Get("signature").String())
				}
			}
		}
		if got := strings.Join(sigs, ","); got != c.want {
			t.Errorf("%s: replayed signatures %q, want %q: %s", c.name, got, c.want, gjson.GetBytes(body, "messages"))
		}
	}
}
