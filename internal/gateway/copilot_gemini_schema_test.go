package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// With tools enabled, Copilot Gemini rejects Codex's disjoint anyOf.
// oneOf preserves the arguments because the required type values differ.
// The earlier Azure turn includes a custom tool call and its output.
func TestCopilotGeminiKeepsCodexToolUnions(t *testing.T) {
	const body = `{
		"model":"copilot/gemini-4-argon","stream":false,"max_output_tokens":32,
		"tool_choice":"auto",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"Apply a patch."},
			{"type":"namespace","name":"mcp__codex_app","tools":[
				{"type":"function","name":"fork_thread","parameters":{
					"type":"object","properties":{"environment":{"anyOf":[
						{"type":"object","properties":{"type":{"type":"string","enum":["same-directory"]}},"required":["type"],"additionalProperties":false},
						{"type":"object","properties":{"type":{"type":"string","enum":["worktree"]}},"required":["type"],"additionalProperties":false}
					]}},"additionalProperties":false
				}}
			]}
		],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Apply the patch."}]},
			{"type":"custom_tool_call","call_id":"patch1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
			{"type":"custom_tool_call_output","call_id":"patch1","output":"Success."},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply COMPAT_OK."}]}
		]
	}`
	for _, c := range []struct {
		name, model, account string
		typed                bool
		union                string
	}{
		{"Copilot Gemini", "gemini-4-argon", "copilot", false, "oneOf"},
		{"Copilot GPT", "gpt-6-astra", "copilot", false, "anyOf"},
		{"Gemini relay", "gemini-4-argon", "", true, "anyOf"},
		{"GPT relay", "gpt-6-astra", "", false, "anyOf"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fresh(t)
			p := provider.Provider{ID: "test", Name: c.name, Chat: "https://schema.test/v1", Key: "test"}
			if c.account != "" {
				p.Account = &provider.Account{Agent: c.account}
			}
			req, err := parseResponses([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			s := New()
			calls := 0
			var sent []byte
			s.client = &http.Client{Transport: countTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				sent, _ = io.ReadAll(r.Body)
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("wrong upstream path: %s", r.URL.Path)
				}
				for _, tool := range gjson.GetBytes(sent, "tools").Array() {
					if tool.Get("function.name").String() != "mcp__codex_app__fork_thread" {
						continue
					}
					env := tool.Get("function.parameters.properties.environment")
					if env.Get("type").Exists() != c.typed ||
						len(env.Get(c.union).Array()) != 2 {
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}},
							Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_request_body","message":"invalid request body"}}`))}, nil
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"COMPAT_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2}}`))}, nil
			})}
			res, proto, err := s.forwardTranslated(context.Background(), p, provider.Chat, req, c.model, http.Header{})
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			reply, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || proto != provider.Chat || calls != 1 || !strings.Contains(string(reply), "COMPAT_OK") {
				t.Fatalf("translated request: status %d, protocol %s, calls %d, reply %s", res.StatusCode, proto, calls, reply)
			}
			tools := gjson.GetBytes(sent, "tools").Array()
			if len(tools) != 2 || tools[0].Get("function.name").String() != "apply_patch" ||
				tools[1].Get("function.name").String() != "mcp__codex_app__fork_thread" {
				t.Fatalf("custom or namespaced tool lost: %s", gjson.GetBytes(sent, "tools"))
			}
			env := tools[1].Get("function.parameters.properties.environment")
			if len(env.Get(c.union).Array()) != 2 ||
				len(env.Get("anyOf").Array())+len(env.Get("oneOf").Array()) != 2 {
				t.Fatalf("union meaning changed: %s", env)
			}
			if gjson.GetBytes(sent, "tool_choice").String() != "auto" {
				t.Fatalf("tools disabled: %s", gjson.GetBytes(sent, "tool_choice"))
			}
			messages := gjson.GetBytes(sent, "messages").Array()
			if len(messages) != 4 || messages[1].Get("tool_calls.0.function.name").String() != "apply_patch" ||
				messages[2].Get("role").String() != "tool" || messages[2].Get("tool_call_id").String() != "patch1" {
				t.Fatalf("earlier custom tool exchange lost: %s", gjson.GetBytes(sent, "messages"))
			}
		})
	}
}
