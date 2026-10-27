package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// These are the rejected Codex shapes. The two review branches share a tag,
// but only one accepts baseBranch, which that branch requires.
const copilotOpenTarget = `{"anyOf":[
	{"type":"object","properties":{"type":{"type":"string","enum":["file"]},"path":{"type":"string"},"line":{"type":"integer"}},"required":["type","path"],"additionalProperties":false},
	{"type":"object","properties":{"type":{"type":"string","enum":["browser"]},"url":{"type":"string"},"tabId":{"type":"string"}},"required":["type"],"additionalProperties":false},
	{"type":"object","properties":{"type":{"type":"string","enum":["terminal"]},"sessionId":{"type":"string"}},"required":["type"],"additionalProperties":false},
	{"type":"object","properties":{"type":{"type":"string","enum":["review"]},"path":{"type":"string"},"view":{"type":"string"}},"required":["type"],"additionalProperties":false},
	{"type":"object","properties":{"type":{"type":"string","enum":["review"]},"baseBranch":{"type":"string"},"path":{"type":"string"},"view":{"type":"string"}},"required":["type","baseBranch"],"additionalProperties":false}
]}`

const copilotCreateTarget = `{"anyOf":[
	{"type":"object","properties":{"type":{"type":"string","enum":["project"]},"projectId":{"type":"string"},"environment":{"anyOf":[
		{"type":"object","properties":{"type":{"type":"string","enum":["local"]}},"required":["type"],"additionalProperties":false},
		{"type":"object","properties":{"type":{"type":"string","enum":["worktree"]},"startingState":{"anyOf":[
			{"type":"object","properties":{"type":{"type":"string","enum":["working-tree"]}},"required":["type"],"additionalProperties":false},
			{"type":"object","properties":{"type":{"type":"string","enum":["branch"]},"branchName":{"type":"string"}},"required":["type","branchName"],"additionalProperties":false}
		]}},"required":["type"],"additionalProperties":false}
	]}},"required":["type","projectId","environment"],"additionalProperties":false},
	{"type":"object","properties":{"type":{"type":"string","enum":["projectless"]},"directoryName":{"type":"string"}},"required":["type"],"additionalProperties":false},
	{"type":"object","properties":{"type":{"type":"string","enum":["chatgptWorkCloud"]},"projectId":{"type":"string"}},"required":["type"],"additionalProperties":false}
]}`

func TestCopilotGeminiAutoTools(t *testing.T) {
	fresh(t)
	body := fmt.Sprintf(`{
		"model":"copilot/gemini-4-argon","stream":false,"max_output_tokens":100,
		"tool_choice":"auto","parallel_tool_calls":true,
		"tools":[{"type":"namespace","name":"mcp__codex_app","tools":[
			{"type":"function","name":"create_thread","parameters":{"type":"object","properties":{"target":%s},"required":["target"],"additionalProperties":false}},
			{"type":"function","name":"open_in_codex","parameters":{"type":"object","properties":{"target":%s},"required":["target"],"additionalProperties":false}}
		]}],
		"input":[{"role":"user","content":"Reply with only 40."}]
	}`, copilotCreateTarget, copilotOpenTarget)
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
		// The live Copilot endpoint rejects either original tool when auto
		// enables tools. Returning 200 for tool_choice:none would hide it.
		if gjson.GetBytes(sent, "tool_choice").String() != "auto" {
			t.Error("tool execution was disabled")
		}
		for _, tool := range gjson.GetBytes(sent, "tools").Array() {
			target := tool.Get("function.parameters.properties.target")
			if target.Get("anyOf").Exists() {
				return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_request_body","message":"invalid request body"}}`))}, nil
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"40"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1}}`))}, nil
	})}
	p := provider.Provider{ID: "copilot", Name: "Copilot", Chat: "https://schema.test/v1",
		Key: "test", Account: &provider.Account{Agent: "copilot"}}
	response, _, err := s.forwardTranslated(context.Background(), p, provider.Chat, req, "gemini-4-argon", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || calls != 1 {
		t.Fatalf("status %d, upstream calls %d", response.StatusCode, calls)
	}
	tools := gjson.GetBytes(sent, "tools").Array()
	if len(tools) != 2 || tools[0].Get("function.name").String() != "mcp__codex_app__create_thread" ||
		tools[1].Get("function.name").String() != "mcp__codex_app__open_in_codex" {
		t.Fatalf("tool definitions changed: %s", gjson.GetBytes(sent, "tools"))
	}
	for i, schema := range []string{copilotCreateTarget, copilotOpenTarget} {
		want := strings.ReplaceAll(schema, `"anyOf"`, `"oneOf"`)
		got := tools[i].Get("function.parameters.properties.target").Raw
		sameJSON(t, []byte(got), want)
	}
}

func TestCopilotGeminiUnionBoundaries(t *testing.T) {
	const a = `{"type":"object","properties":{"tag":{"const":"a"}},"required":["tag"]}`
	const b = `{"type":"object","properties":{"tag":{"enum":["b","c"]}},"required":["tag"]}`
	disjoint := `{"anyOf":[` + a + `,` + b + `]}`
	for _, c := range []struct {
		name, schema, want string
	}{
		{"required const and enum", disjoint, strings.ReplaceAll(disjoint, `"anyOf"`, `"oneOf"`)},
		{"nested create target", copilotCreateTarget, strings.ReplaceAll(copilotCreateTarget, `"anyOf"`, `"oneOf"`)},
		{"closed review branches", copilotOpenTarget, strings.ReplaceAll(copilotOpenTarget, `"anyOf"`, `"oneOf"`)},
		{"overlapping enum", `{"anyOf":[` + a + `,{"type":"object","properties":{"tag":{"enum":["a","b"]}},"required":["tag"]}]}`, ""},
		{"optional discriminator", `{"anyOf":[` + a + `,{"type":"object","properties":{"tag":{"const":"b"}}}]}`, ""},
		{"open review branch", `{"anyOf":[{"type":"object","properties":{"type":{"const":"review"}},"required":["type"]},{"type":"object","properties":{"type":{"const":"review"},"baseBranch":{"type":"string"}},"required":["type","baseBranch"],"additionalProperties":false}]}`, ""},
		{"pattern permits required field", `{"anyOf":[{"type":"object","properties":{},"patternProperties":{".*":{"type":"string"}},"additionalProperties":false},{"type":"object","properties":{"baseBranch":{"type":"string"}},"required":["baseBranch"]}]}`, ""},
		{"branch ref siblings", `{"anyOf":[{"$ref":"#/$defs/shared","type":"object","properties":{"tag":{"const":"a"}},"required":["tag"]},` + b + `]}`, ""},
		{"tag ref siblings", `{"anyOf":[{"type":"object","properties":{"tag":{"$ref":"#/$defs/shared","const":"a"}},"required":["tag"]},` + b + `]}`, ""},
		{"nullable union", `{"anyOf":[{"type":"string"},{"type":"null"}]}`, ""},
		{"existing oneOf", `{"anyOf":[` + a + `,` + b + `],"oneOf":[{"type":"object"}]}`, ""},
		{"property named anyOf", `{"type":"object","properties":{"anyOf":` + disjoint + `}}`, `{"type":"object","properties":{"anyOf":` + strings.ReplaceAll(disjoint, `"anyOf"`, `"oneOf"`) + `}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.want == "" {
				c.want = c.schema
			}
			wrap := func(schema, model string) []byte {
				return []byte(fmt.Sprintf(`{"model":%q,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"fixture","parameters":%s}}]}`, model, schema))
			}
			p := provider.Provider{Account: &provider.Account{Agent: "copilot"}}
			body, want := wrap(c.schema, "gemini-4-argon"), wrap(c.want, "gemini-4-argon")
			if got := copilotGeminiToolUnions(p, body); string(got) != string(want) {
				t.Fatalf("got %s, want %s", got, want)
			}
			// Provider and model boundaries leave byte-identical requests.
			if got := copilotGeminiToolUnions(provider.Provider{}, body); string(got) != string(body) {
				t.Fatalf("changed non-Copilot body: %s", got)
			}
			gpt := wrap(c.schema, "gpt-6-astra")
			if got := copilotGeminiToolUnions(p, gpt); string(got) != string(gpt) {
				t.Fatalf("changed GPT body: %s", got)
			}
		})
	}
}
