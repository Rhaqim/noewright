package engine

// bundle_schema_test.go — the embedded JSON Schema must describe every field
// a bundle file can hold, so it cannot drift from the Go types.

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBundleSchemaMatchesTypes(t *testing.T) {
	var doc struct {
		Properties map[string]any `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(BundleSchema(), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props := func(def string) map[string]any {
		if def == "" {
			return doc.Properties
		}
		d, ok := doc.Defs[def]
		if !ok {
			t.Fatalf("schema has no $defs/%s", def)
		}
		return d.Properties
	}
	// Each Go type a bundle file holds, and the schema definition for it.
	cases := []struct {
		v   any
		def string
	}{
		{Bundle{}, ""}, {BundleRequires{}, "requires"}, {BundleGenerator{}, "generator"},
		{BundleResponseFormat{}, "response_format"}, {BundlePrompt{}, "prompt"}, {BundleAgent{}, "agent"},
		{GenerateParams{}, "params"}, {ResponseFormat{}, "inline_response_format"},
		{BundleFlow{}, "flow"}, {BundleFlowAgent{}, "flow_agent"}, {BundleGraph{}, "graph_record"},
		{Graph{}, "graph"}, {GraphVar{}, "graph_var"}, {GraphNode{}, "node"}, {FlowAgent{}, "node_agent"},
		{GraphDecision{}, "decide"}, {GraphWait{}, "wait"}, {GraphEdge{}, "edge"},
		{BundleTest{}, "test"}, {BundleTestStep{}, "test_step"}, {BundleExpect{}, "expect"}, {BundleEvidence{}, "evidence"},
	}
	for _, c := range cases {
		var have map[string]any
		if c.def == "inline_response_format" {
			agent := props("agent")
			have = agent["inline_response_format"].(map[string]any)["properties"].(map[string]any)
		} else {
			have = props(c.def)
		}
		typ := reflect.TypeOf(c.v)
		var fields []string
		for i := range typ.NumField() {
			tag := typ.Field(i).Tag.Get("yaml")
			name, _, _ := strings.Cut(tag, ",")
			if name == "-" || !typ.Field(i).IsExported() {
				continue
			}
			if name == "" {
				name = strings.ToLower(typ.Field(i).Name) // yaml.v3's default
			}
			fields = append(fields, name)
			if _, ok := have[name]; !ok {
				t.Errorf("%s field %q (yaml %q) is missing from the schema's %q", typ.Name(), typ.Field(i).Name, name, c.def)
			}
		}
		for name := range have {
			if !slices.Contains(fields, name) {
				t.Errorf("schema %q lists %q, which %s does not have", c.def, name, typ.Name())
			}
		}
	}
}
