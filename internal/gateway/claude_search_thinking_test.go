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
