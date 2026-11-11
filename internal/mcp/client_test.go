package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-valley/pi/ai"

	"piui/internal/store"
)

func TestHelperMCPStdioProcess(t *testing.T) {
	if os.Getenv("PIUI_TEST_MCP_STDIO_HELPER") != "1" {
		return
	}
	secretEnv := os.Getenv("MCP_CUSTOM_TOKEN")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      *int64         `json:"id"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		writeResp := func(result any) {
			raw, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result":  result,
			})
			_, _ = fmt.Fprintf(os.Stdout, "%s\n", raw)
		}
		switch req.Method {
		case "initialize":
			writeResp(map[string]any{
				"protocolVersion": ProtocolVersion,
				"serverInfo": map[string]any{
					"name":    "mock-stdio-mcp",
					"version": "1.2.0",
				},
				"capabilities": map[string]any{"tools": map[string]any{}},
			})
		case "tools/list":
			writeResp(map[string]any{
				"tools": []map[string]any{
					{
						"name":        "lookup_ticket",
						"description": "Lookup engineering ticket by ID",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"ticket_id": map[string]any{"type": "string", "description": "Ticket key"},
							},
							"required": []string{"ticket_id"},
						},
					},
				},
			})
		case "tools/call":
			args, _ := req.Params["arguments"].(map[string]any)
			tid, _ := args["ticket_id"].(string)
			writeResp(map[string]any{
				"content": []map[string]any{
					{
						"type": "text",
						"text": fmt.Sprintf("Ticket %s: READY (env=%s)", tid, secretEnv),
					},
				},
			})
		}
	}
	os.Exit(0)
}

func TestMCPStdioAndStreamableHTTPHubBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Mock Streamable HTTP MCP server returning SSE for tools/call and JSON for initialize/list
	var gotAuthHeader, gotSessionID string
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		gotAuthHeader = r.Header.Get("Authorization")
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			gotSessionID = sid
		}
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-mcp-42")
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"protocolVersion": ProtocolVersion,
					"serverInfo":      map[string]any{"name": "mock-http-docs", "version": "2.0.0"},
				},
			})
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "search_docs",
							"description": "Search internal docs over Streamable HTTP",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"query": map[string]any{"type": "string"},
								},
								"required": []string{"query"},
							},
						},
					},
				},
			})
		case "tools/call":
			args, _ := req.Params["arguments"].(map[string]any)
			q, _ := args["query"].(string)
			w.Header().Set("Content-Type", "text/event-stream")
			respBytes, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "HTTP Doc Hit for: " + q},
					},
				},
			})
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", respBytes)
		}
	}))
	defer httpSrv.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	configs := []store.MCPServerConfig{
		{
			ID:        "stdio-1",
			Name:      "jira-local",
			Transport: store.MCPTransportStdio,
			Enabled:   true,
			Command:   exe,
			Args:      []string{"-test.run=TestHelperMCPStdioProcess", "--"},
			Env: map[string]string{
				"PIUI_TEST_MCP_STDIO_HELPER": "1",
				"MCP_CUSTOM_TOKEN":           "sec-99",
			},
		},
		{
			ID:          "http-1",
			Name:        "wiki-http",
			Transport:   store.MCPTransportHTTP,
			Enabled:     true,
			URL:         httpSrv.URL,
			BearerToken: "mcp-secret-bearer",
		},
	}

	hub := NewHub()
	defer func() { _ = hub.Close() }()

	statuses := hub.Sync(ctx, configs, t.TempDir())
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	for _, st := range statuses {
		if !st.Connected {
			t.Fatalf("expected server %s connected, got error: %s", st.Name, st.Error)
		}
	}
	if hub.ActiveToolCount() != 2 {
		t.Fatalf("expected 2 active MCP tools, got %d", hub.ActiveToolCount())
	}

	agentTools := hub.BuildAgentTools()
	if len(agentTools) != 2 {
		t.Fatalf("expected 2 bridged AgentTools, got %d", len(agentTools))
	}

	// Execute Stdio tool
	res0, err := agentTools[0].Execute(ctx, "call-1", map[string]any{"ticket_id": "ENG-108"}, nil)
	if err != nil {
		t.Fatalf("stdio Execute failed: %v", err)
	}
	txt0 := ai.ContentText(res0.Content, "\n")
	if !strings.Contains(txt0, "Ticket ENG-108: READY (env=sec-99)") {
		t.Fatalf("unexpected stdio tool output: %q", txt0)
	}

	// Execute Streamable HTTP SSE tool
	res1, err := agentTools[1].Execute(ctx, "call-2", map[string]any{"query": "raft consensus"}, nil)
	if err != nil {
		t.Fatalf("http Execute failed: %v", err)
	}
	txt1 := ai.ContentText(res1.Content, "\n")
	if !strings.Contains(txt1, "HTTP Doc Hit for: raft consensus") {
		t.Fatalf("unexpected http tool output: %q", txt1)
	}
	if gotAuthHeader != "Bearer mcp-secret-bearer" {
		t.Fatalf("expected Authorization Bearer header, got %q", gotAuthHeader)
	}
	if gotSessionID != "sess-mcp-42" {
		t.Fatalf("expected Mcp-Session-Id propagation, got %q", gotSessionID)
	}
}
