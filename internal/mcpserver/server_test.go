package mcpserver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/swiftugandan/tallyfold/internal/memory"
)

// Claude Code cuts server instructions at 2,048 characters.
func TestInstructionsFitClaudeCode(t *testing.T) {
	for name, s := range map[string]string{"instructions": instructions, "notSetUp": notSetUp} {
		if len(s) > 2048 {
			t.Errorf("%s is %d characters; Claude Code shows only 2,048", name, len(s))
		}
	}
}

// A client session sets up memory and writes through the tools; the entry names
// the client that wrote it.
func TestToolsOverAClientSession(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ctx := context.Background()
	store := memory.Open(t.TempDir())
	server := New(store, "test")

	client := mcp.NewClient(&mcp.Implementation{Name: "Claude Code", Version: "2"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String(), res.IsError
	}

	if out, isErr := call("memory_context", nil); !isErr || !strings.Contains(out, "memory_init") {
		t.Fatalf("context before setup = %q (error %v), want a pointer to memory_init", out, isErr)
	}
	if out, isErr := call("memory_init", map[string]any{}); isErr {
		t.Fatalf("memory_init: %s", out)
	}
	if out, isErr := call("memory_write", map[string]any{"action": "add", "text": "Prefers tables for comparisons"}); isErr {
		t.Fatalf("memory_write: %s", out)
	}
	out, _ := call("memory_context", nil)
	if !strings.Contains(out, "- Prefers tables for comparisons [added: ") || !strings.Contains(out, "by: claude-code]") {
		t.Fatalf("context after write =\n%s", out)
	}
	if out, isErr := call("memory_write", map[string]any{"action": "add", "text": "x", "scope": "team"}); !isErr || !strings.Contains(out, "not supported") {
		t.Fatalf("team write = %q (error %v)", out, isErr)
	}
}
