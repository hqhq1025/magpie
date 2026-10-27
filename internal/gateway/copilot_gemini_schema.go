package gateway

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// Copilot's Gemini rejects some anyOf tool schemas while tools are enabled.
// Disjoint required discriminators, or required fields forbidden by another
// branch, prove that at most one branch can match. oneOf then preserves the
// accepted arguments.
func copilotGeminiToolUnions(p provider.Provider, body []byte) []byte {
	if p.Account == nil || p.Account.Agent != "copilot" ||
		!geminiNamed(gjson.GetBytes(body, "model").String()) {
		return body
	}
	var at []int
	gjson.GetBytes(body, "tools").ForEach(func(_, tool gjson.Result) bool {
		for _, path := range []string{"function.parameters", "parameters", "input_schema"} {
			collectCopilotUnions(body, tool.Get(path), &at)
		}
		return true
	})
	if len(at) == 0 {
		return body
	}
	out := bytes.Clone(body)
	for _, i := range at {
		copy(out[i:], `"oneOf"`)
	}
	return out
}

func collectCopilotUnions(body []byte, node gjson.Result, at *[]int) {
	if node.IsArray() {
		node.ForEach(func(_, child gjson.Result) bool {
			collectCopilotUnions(body, child, at)
			return true
		})
		return
	}
	if !node.IsObject() {
		return
	}
	if union := node.Get("anyOf"); !node.Get("oneOf").Exists() && disjointObjectBranches(union) {
		if i := keyBefore(body, union.Index, `"anyOf"`); i >= 0 {
			*at = append(*at, i)
		}
	}
	node.ForEach(func(key, child gjson.Result) bool {
		switch key.String() {
		case "properties", "$defs", "definitions", "dependentSchemas", "patternProperties":
			child.ForEach(func(_, schema gjson.Result) bool {
				collectCopilotUnions(body, schema, at)
				return true
			})
		case "items", "additionalProperties", "contains", "propertyNames", "not", "if", "then", "else", "allOf", "anyOf", "oneOf", "prefixItems", "unevaluatedProperties", "unevaluatedItems", "contentSchema":
			collectCopilotUnions(body, child, at)
		}
		return true
	})
}

func disjointObjectBranches(union gjson.Result) bool {
	branches := union.Array()
	if !union.IsArray() || len(branches) < 2 {
		return false
	}
	for i, a := range branches {
		if a.Get("type").String() != "object" || a.Get(`\$ref`).Exists() {
			return false
		}
		for _, b := range branches[:i] {
			if !objectBranchesDiffer(a, b) && !objectBranchesDiffer(b, a) {
				return false
			}
		}
	}
	return true
}

func objectBranchesDiffer(a, b gjson.Result) bool {
	bProps := b.Get("properties").Map()
	for _, name := range a.Get("required").Array() {
		if name.Type != gjson.String {
			continue
		}
		field := name.String()
		bProp, exists := bProps[field]
		if !exists && b.Get("additionalProperties").Type == gjson.False &&
			!b.Get("patternProperties").Exists() {
			return true
		}
		// The field must occur in every value accepted by either branch.
		required := false
		for _, other := range b.Get("required").Array() {
			required = required || other.Type == gjson.String && other.String() == field
		}
		av, bv := schemaStringValues(a.Get("properties").Map()[field]), schemaStringValues(bProp)
		if !required || len(av) == 0 || len(bv) == 0 {
			continue
		}
		overlap := false
		for value := range av {
			overlap = overlap || bv[value]
		}
		if !overlap {
			return true
		}
	}
	return false
}

func schemaStringValues(schema gjson.Result) map[string]bool {
	if schema.Get(`\$ref`).Exists() {
		return nil
	}
	values := schema.Get("enum").Array()
	if value := schema.Get("const"); value.Type == gjson.String {
		values = []gjson.Result{value}
	}
	out := map[string]bool{}
	for _, value := range values {
		if value.Type != gjson.String {
			return nil
		}
		out[value.String()] = true
	}
	return out
}
