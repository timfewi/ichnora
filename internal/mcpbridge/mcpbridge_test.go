package mcpbridge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/timfewi/ichnora/internal/contract"
	"github.com/timfewi/ichnora/internal/server"
)

// connect returns a client of the bridge and the resolved input schema of
// every advertised tool.
func connect(t *testing.T) (*mcp.ClientSession, map[string]*jsonschema.Resolved) {
	t.Helper()
	ctx := t.Context()
	st, ct := mcp.NewInMemoryTransports()
	s := Server("test", server.Client{}, func() *contract.SessionHint { return nil })
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	schemas := map[string]*jsonschema.Resolved{}
	for tool, err := range client.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if schemas[tool.Name], err = schema.Resolve(nil); err != nil {
			t.Fatal(err)
		}
	}
	return client, schemas
}

func TestAdvertisedSearchBudget(t *testing.T) {
	client, schemas := connect(t)
	resolved := schemas["memory_search"]
	if resolved == nil {
		t.Fatal("memory_search missing from tools/list")
	}
	args := map[string]any{"query": "benchmark luna"}
	if err := resolved.Validate(args); err != nil {
		t.Fatalf("omitting the budget: %v", err)
	}
	for _, n := range []int{0, 1000, contract.MinBudget, contract.MaxBudget, contract.MaxBudget + 1} {
		args["max_bytes"] = n
		_, budgetErr := contract.Budget(n)
		if schemaErr := resolved.Validate(args); (schemaErr == nil) != (budgetErr == nil) {
			t.Errorf("budget %d: schema error %v, contract error %v", n, schemaErr, budgetErr)
		}
	}
	args["max_bytes"] = 1000
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "memory_search", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("expected MCP validation error for 1000 bytes: %+v", result)
	}
	message, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(message.Text, "max_bytes") {
		t.Fatalf("expected validation error to identify max_bytes: %+v", result.Content[0])
	}
}

// The schemas name the values of docs/contract.md. An optional field keeps
// accepting the empty string that the service reads as omitted.
func TestAdvertisedValues(t *testing.T) {
	_, schemas := connect(t)
	claim := func(field string, value any) map[string]any {
		return map[string]any{"claims": []any{map[string]any{"kind": "decision", "title": "t", field: value}}}
	}
	ref := func(role string) func(string) map[string]any {
		return func(kind string) map[string]any { return claim(role, []any{map[string]any{"kind": kind}}) }
	}
	task := func(field string) func(string) map[string]any {
		return func(value string) map[string]any { return map[string]any{"action": "list", field: value} }
	}
	for _, c := range []struct {
		tool, field string
		args        func(string) map[string]any
		values      string
		optional    bool
	}{
		{"memory_search", "view", func(v string) map[string]any { return map[string]any{"query": "", "view": v} }, "working history all review", true},
		{"memory_search", "kinds", func(v string) map[string]any { return map[string]any{"query": "", "kinds": []any{v}} }, "observation decision summary note source", false},
		{"memory_capture", "kind", func(v string) map[string]any { return claim("kind", v) }, "observation decision summary note", false},
		{"memory_capture", "status", func(v string) map[string]any { return claim("status", v) }, "observed inferred", true},
		{"memory_capture", "support", ref("support"), "file ast json_field tabula event turn claim checkpoint attempt check external", false},
		{"memory_capture", "depends", ref("depends"), "file ast json_field tabula revision environment", false},
		{"memory_update", "status", func(v string) map[string]any { return map[string]any{"id": "clm_x", "status": v, "reason": "r"} }, "disputed invalidated", false},
		{"memory_task", "action", func(v string) map[string]any { return map[string]any{"action": v} }, "list create checkpoint transition resume", false},
		{"memory_task", "to", task("to"), "active blocked completed cancelled reopened", true},
		{"memory_task", "outcome", func(v string) map[string]any {
			check := map[string]any{"check": "c", "outcome": v, "inputs": map[string]any{}}
			return map[string]any{"action": "checkpoint", "checkpoint": map[string]any{"verification": []any{check}}}
		}, "passed failed skipped", false},
		{"memory_sim", "action", func(v string) map[string]any { return map[string]any{"action": v, "problem": "p"} }, "history propose observe", false},
	} {
		schema := schemas[c.tool]
		for _, value := range strings.Fields(c.values) {
			if err := schema.Validate(c.args(value)); err != nil {
				t.Errorf("%s %s %q: %v", c.tool, c.field, value, err)
			}
		}
		if err := schema.Validate(c.args("unlisted")); err == nil {
			t.Errorf("%s %s accepts an unlisted value", c.tool, c.field)
		}
		if err := schema.Validate(c.args("")); (err == nil) != c.optional {
			t.Errorf("%s %s empty string: optional %v, schema error %v", c.tool, c.field, c.optional, err)
		}
	}
}
