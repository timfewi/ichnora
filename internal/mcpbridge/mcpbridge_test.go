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

func TestAdvertisedSearchBudget(t *testing.T) {
	ctx := t.Context()
	st, ct := mcp.NewInMemoryTransports()
	s := Server("test", server.Client{}, func() *contract.SessionHint { return nil })
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for tool, err := range client.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name != "memory_search" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
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
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
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
		return
	}
	t.Fatal("memory_search missing from tools/list")
}
