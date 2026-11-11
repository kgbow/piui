package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"piui/internal/store"
)

const (
	// ProtocolVersion is the MCP JSON-RPC protocol version negotiated during initialize.
	ProtocolVersion = "2024-11-05"
)

// ToolSpec describes one tool exposed by an MCP server via tools/list.
type ToolSpec struct {
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	InputSchema     json.RawMessage `json:"inputSchema,omitempty"`
	ReadOnlyHint    bool            `json:"-"`
	DestructiveHint bool            `json:"-"`
}

// ContentItem represents a single content block returned by MCP tools/call.
type ContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// CallResult represents the decoded response of MCP tools/call.
type CallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ServerStatus summarizes the live connection state and discovered tools of an MCP server.
type ServerStatus struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Transport     store.MCPTransport `json:"transport"`
	Enabled       bool              `json:"enabled"`
	Connected     bool              `json:"connected"`
	ServerName    string            `json:"server_name,omitempty"`
	ServerVersion string            `json:"server_version,omitempty"`
	Tools         []ToolSpec        `json:"tools,omitempty"`
	Error         string            `json:"error,omitempty"`
	LatencyMs     int64             `json:"latency_ms,omitempty"`
	CheckedAt     time.Time         `json:"checked_at"`
}

// Client is the common transport interface implemented by StdioClient and HTTPClient.
type Client interface {
	ServerInfo() (name, version string)
	ListTools(ctx context.Context) ([]ToolSpec, error)
	CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error)
	Close() error
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type initParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type rawToolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
	} `json:"annotations,omitempty"`
}

type listToolsResult struct {
	Tools      []rawToolWire `json:"tools"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// Dial connects and initializes either a StdioClient or HTTPClient based on cfg.Transport.
func Dial(ctx context.Context, cfg store.MCPServerConfig, workDir string) (Client, error) {
	if cfg.Transport == store.MCPTransportHTTP {
		return NewHTTPClient(ctx, cfg)
	}
	return NewStdioClient(ctx, cfg, workDir)
}

// StdioClient speaks MCP JSON-RPC 2.0 over a local child process's stdin/stdout pipes.
type StdioClient struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	writeMu    sync.Mutex
	nextID     atomic.Int64
	mu         sync.Mutex
	pending    map[int64]chan rpcEnvelope
	closed     bool
	closeErr   error
	stderrBuf  bytes.Buffer
	stderrMu   sync.Mutex
	srvName    string
	srvVersion string
	doneCh     chan struct{}
}

// NewStdioClient spawns the MCP stdio subprocess and completes the initialize handshake.
func NewStdioClient(ctx context.Context, cfg store.MCPServerConfig, workDir string) (*StdioClient, error) {
	command := strings.TrimSpace(cfg.Command)
	args := cfg.Args
	if command == "" {
		return nil, errors.New("MCP Stdio 命令不能为空")
	}
	// Support users pasting full command+args into Command when Args is empty
	if len(args) == 0 && strings.ContainsAny(command, " \t") {
		command, args = store.ParseCommandLine(command)
	}
	command = store.ExpandLocalPath(command)

	cmd := exec.Command(command, args...)
	if wd := strings.TrimSpace(workDir); wd != "" {
		expandedWd := store.ExpandLocalPath(wd)
		if info, err := os.Stat(expandedWd); err == nil && info.IsDir() {
			cmd.Dir = expandedWd
		}
	}

	env := os.Environ()
	for k, v := range cfg.Env {
		if strings.TrimSpace(k) != "" {
			env = append(env, strings.TrimSpace(k)+"="+v)
		}
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 MCP stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 MCP stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("创建 MCP stderr 管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 进程 %q 失败: %w", command, err)
	}

	c := &StdioClient{
		cmd:     cmd,
		stdin:   stdin,
		pending: make(map[int64]chan rpcEnvelope),
		doneCh:  make(chan struct{}),
	}

	go c.captureStderr(stderr)
	go c.readLoop(stdout)

	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *StdioClient) captureStderr(r io.Reader) {
	buf := make([]byte, 2048)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.stderrMu.Lock()
			if c.stderrBuf.Len() < 8192 {
				_, _ = c.stderrBuf.Write(buf[:n])
			}
			c.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *StdioClient) stderrSnippet() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	s := strings.TrimSpace(c.stderrBuf.String())
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

func (c *StdioClient) readLoop(r io.Reader) {
	defer close(c.doneCh)
	br := bufio.NewReaderSize(r, 64*1024)

	for {
		payload, err := readRPCFrame(br)
		if err != nil {
			c.failAllPending(err)
			return
		}
		if len(payload) == 0 {
			continue
		}
		var env rpcEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			continue
		}
		// Respond to server-initiated ping requests so strict servers stay healthy
		if env.Method == "ping" && env.ID != nil {
			_ = c.writeRawJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      *env.ID,
				"result":  map[string]any{},
			})
			continue
		}
		if env.ID == nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[*env.ID]
		if ok {
			delete(c.pending, *env.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- env
		}
	}
}

// readRPCFrame supports both MCP standard newline-delimited JSON frames and Content-Length headers.
func readRPCFrame(br *bufio.Reader) ([]byte, error) {
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		if bytes.HasPrefix(bytes.ToLower(trimmed), []byte("content-length:")) {
			valStr := strings.TrimSpace(string(trimmed[len("content-length:"):]))
			length, convErr := strconv.Atoi(valStr)
			if convErr != nil || length < 0 || length > 16*1024*1024 {
				return nil, fmt.Errorf("invalid MCP Content-Length %q", valStr)
			}
			// Consume remaining headers up to blank line
			for {
				hdr, hdrErr := br.ReadBytes('\n')
				if hdrErr != nil {
					return nil, hdrErr
				}
				if len(bytes.TrimSpace(hdr)) == 0 {
					break
				}
			}
			body := make([]byte, length)
			if _, readErr := io.ReadFull(br, body); readErr != nil {
				return nil, readErr
			}
			return body, nil
		}
		return trimmed, nil
	}
}

func (c *StdioClient) failAllPending(err error) {
	c.mu.Lock()
	c.closed = true
	if c.closeErr == nil {
		c.closeErr = err
	}
	pending := c.pending
	c.pending = make(map[int64]chan rpcEnvelope)
	c.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
}

func (c *StdioClient) writeRawJSON(msg any) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(raw)
	return err
}

func (c *StdioClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcEnvelope, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("MCP 连接已关闭")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	if err := c.writeRawJSON(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("发送 MCP 请求 %s 失败: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case env, ok := <-ch:
		if !ok {
			if stderr := c.stderrSnippet(); stderr != "" {
				return fmt.Errorf("MCP 进程意外退出: %s", stderr)
			}
			return errors.New("MCP 进程连接已断开")
		}
		if env.Error != nil {
			return fmt.Errorf("MCP 错误 (%d): %s", env.Error.Code, env.Error.Message)
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("解析 MCP %s 响应失败: %w", method, err)
			}
		}
		return nil
	}
}

func (c *StdioClient) initialize(ctx context.Context) error {
	var p initParams
	p.ProtocolVersion = ProtocolVersion
	p.Capabilities = map[string]any{}
	p.ClientInfo.Name = "PiUI"
	p.ClientInfo.Version = "1.0.0"

	var res initResult
	if err := c.call(ctx, "initialize", p, &res); err != nil {
		return err
	}
	c.srvName = res.ServerInfo.Name
	c.srvVersion = res.ServerInfo.Version

	_ = c.writeRawJSON(rpcNotification{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	return nil
}

// ServerInfo returns the remote server name and version reported during initialize.
func (c *StdioClient) ServerInfo() (string, string) {
	return c.srvName, c.srvVersion
}

// ListTools fetches all tools exposed by this stdio MCP server.
func (c *StdioClient) ListTools(ctx context.Context) ([]ToolSpec, error) {
	return listAllTools(ctx, c.call)
}

// CallTool invokes a named tool on this stdio MCP server.
func (c *StdioClient) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res CallResult
	err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, &res)
	return res, err
}

// Close terminates the MCP stdio child process cleanly without leaving orphan processes.
func (c *StdioClient) Close() error {
	c.mu.Lock()
	if c.closed && c.cmd == nil {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		select {
		case <-c.doneCh:
		case <-time.After(300 * time.Millisecond):
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	}
	return nil
}

// HTTPClient speaks MCP Streamable HTTP (JSON-RPC 2.0 over HTTP POST with JSON or SSE responses).
type HTTPClient struct {
	endpoint   string
	headers    map[string]string
	bearer     string
	httpClient *http.Client
	nextID     atomic.Int64
	mu         sync.Mutex
	sessionID  string
	srvName    string
	srvVersion string
}

// NewHTTPClient connects to a Streamable HTTP MCP endpoint and performs the initialize handshake.
func NewHTTPClient(ctx context.Context, cfg store.MCPServerConfig) (*HTTPClient, error) {
	u := strings.TrimSpace(cfg.URL)
	if u == "" {
		return nil, errors.New("MCP HTTP URL 不能为空")
	}
	c := &HTTPClient{
		endpoint: u,
		headers:  cfg.Headers,
		bearer:   strings.TrimSpace(cfg.BearerToken),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *HTTPClient) initialize(ctx context.Context) error {
	var p initParams
	p.ProtocolVersion = ProtocolVersion
	p.Capabilities = map[string]any{}
	p.ClientInfo.Name = "PiUI"
	p.ClientInfo.Version = "1.0.0"

	var res initResult
	if err := c.call(ctx, "initialize", p, &res); err != nil {
		return err
	}
	c.srvName = res.ServerInfo.Name
	c.srvVersion = res.ServerInfo.Version

	_ = c.notify(ctx, "notifications/initialized", nil)
	return nil
}

func (c *HTTPClient) buildRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(strings.TrimSpace(k), v)
		}
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	return req, nil
}

func (c *HTTPClient) notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(rpcNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	req, err := c.buildRequest(ctx, raw)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

func (c *HTTPClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	raw, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	req, err := c.buildRequest(ctx, raw)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求 MCP HTTP %s 失败: %w", method, err)
	}
	defer resp.Body.Close()

	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var env rpcEnvelope
	if strings.Contains(ct, "text/event-stream") {
		parsed, sseErr := readSSEEnvelopeForID(resp.Body, id)
		if sseErr != nil {
			return sseErr
		}
		env = parsed
	} else {
		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
		if readErr != nil {
			return readErr
		}
		if err := json.Unmarshal(bodyBytes, &env); err != nil {
			return fmt.Errorf("解析 MCP HTTP JSON 响应失败: %w", err)
		}
	}

	if env.Error != nil {
		return fmt.Errorf("MCP 错误 (%d): %s", env.Error.Code, env.Error.Message)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("解码 MCP %s 结果失败: %w", method, err)
		}
	}
	return nil
}

func readSSEEnvelopeForID(r io.Reader, targetID int64) (rpcEnvelope, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var dataLines []string

	tryFlush := func() (rpcEnvelope, bool) {
		if len(dataLines) == 0 {
			return rpcEnvelope{}, false
		}
		joined := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if joined == "" {
			return rpcEnvelope{}, false
		}
		var env rpcEnvelope
		if err := json.Unmarshal([]byte(joined), &env); err == nil {
			if env.ID != nil && *env.ID == targetID {
				return env, true
			}
			if env.Error != nil && env.ID == nil {
				return env, true
			}
		}
		return rpcEnvelope{}, false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if env, ok := tryFlush(); ok {
				return env, nil
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(after))
		}
	}
	if env, ok := tryFlush(); ok {
		return env, nil
	}
	if err := scanner.Err(); err != nil {
		return rpcEnvelope{}, err
	}
	return rpcEnvelope{}, errors.New("MCP SSE 流未返回匹配的 JSON-RPC 响应")
}

// ServerInfo returns the remote server name and version reported during initialize.
func (c *HTTPClient) ServerInfo() (string, string) {
	return c.srvName, c.srvVersion
}

// ListTools fetches all tools exposed by this HTTP MCP server.
func (c *HTTPClient) ListTools(ctx context.Context) ([]ToolSpec, error) {
	return listAllTools(ctx, c.call)
}

// CallTool invokes a named tool on this HTTP MCP server.
func (c *HTTPClient) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res CallResult
	err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, &res)
	return res, err
}

// Close releases any HTTP session resources (sending optional DELETE when Mcp-Session-Id is active).
func (c *HTTPClient) Close() error {
	c.mu.Lock()
	sid := c.sessionID
	c.sessionID = ""
	c.mu.Unlock()
	if sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil); err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			if c.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+c.bearer)
			}
			if resp, doErr := c.httpClient.Do(req); doErr == nil {
				_ = resp.Body.Close()
			}
		}
	}
	return nil
}

func listAllTools(ctx context.Context, callFn func(ctx context.Context, method string, params any, out any) error) ([]ToolSpec, error) {
	var all []ToolSpec
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res listToolsResult
		if err := callFn(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		for _, w := range res.Tools {
			if strings.TrimSpace(w.Name) == "" {
				continue
			}
			spec := ToolSpec{
				Name:        strings.TrimSpace(w.Name),
				Description: strings.TrimSpace(w.Description),
				InputSchema: w.InputSchema,
			}
			if w.Annotations != nil {
				if w.Annotations.ReadOnlyHint != nil {
					spec.ReadOnlyHint = *w.Annotations.ReadOnlyHint
				}
				if w.Annotations.DestructiveHint != nil {
					spec.DestructiveHint = *w.Annotations.DestructiveHint
				}
			}
			all = append(all, spec)
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// SanitizeToolSegment converts an arbitrary MCP server or tool name into a provider-safe identifier segment
// matching ^[a-zA-Z0-9_-]+$ required by OpenAI and Anthropic tool APIs.
func SanitizeToolSegment(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "mcp"
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		case r == '-' || r == '.' || r == '/' || r == ' ':
			b.WriteByte('_')
		}
	}
	res := strings.Trim(b.String(), "_")
	if res == "" {
		return "mcp"
	}
	if len(res) > 28 {
		res = res[:28]
	}
	return res
}

// ParseMCPAgentToolName splits "mcp__<server>__<tool>" into server and tool display parts.
func ParseMCPAgentToolName(agentToolName string) (serverSlug, toolSlug string, ok bool) {
	if !strings.HasPrefix(agentToolName, "mcp__") {
		return "", "", false
	}
	rest := strings.TrimPrefix(agentToolName, "mcp__")
	srv, tl, found := strings.Cut(rest, "__")
	if !found || srv == "" || tl == "" {
		return "", "", false
	}
	return srv, tl, true
}

func convertInputSchema(raw json.RawMessage) *ai.Schema {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ai.Object()
	}
	var s ai.Schema
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return ai.Object()
	}
	if s.Type == "" {
		s.Type = "object"
	}
	return &s
}

type managedEntry struct {
	cfg       store.MCPServerConfig
	sig       string
	client    Client
	status    ServerStatus
}

func serverConfigSignature(cfg store.MCPServerConfig) string {
	raw, _ := json.Marshal(cfg)
	return string(raw)
}

// Hub manages live connections to configured MCP servers and bridges their tools into agent.AgentTool.
type Hub struct {
	mu      sync.Mutex
	entries map[string]*managedEntry
	order   []string
}

// NewHub creates an empty MCP connection hub.
func NewHub() *Hub {
	return &Hub{
		entries: make(map[string]*managedEntry),
	}
}

// ProbeServer tests a single MCPServerConfig (used by Settings UI "连接并发现工具") and closes the temporary probe connection.
func ProbeServer(ctx context.Context, cfg store.MCPServerConfig, workDir string) ServerStatus {
	started := time.Now()
	st := ServerStatus{
		ID:        cfg.ID,
		Name:      cfg.Name,
		Transport: cfg.Transport,
		Enabled:   cfg.Enabled,
		CheckedAt: started,
	}
	client, err := Dial(ctx, cfg, workDir)
	if err != nil {
		st.Error = err.Error()
		st.LatencyMs = max(1, time.Since(started).Milliseconds())
		return st
	}
	defer func() { _ = client.Close() }()

	tools, err := client.ListTools(ctx)
	st.LatencyMs = max(1, time.Since(started).Milliseconds())
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Connected = true
	st.ServerName, st.ServerVersion = client.ServerInfo()
	st.Tools = tools
	return st
}

// Sync connects newly enabled or modified MCP servers, reuses unchanged live connections,
// closes removed/disabled servers, and returns a snapshot of all ServerStatuses.
func (h *Hub) Sync(ctx context.Context, configs []store.MCPServerConfig, workDir string) []ServerStatus {
	h.mu.Lock()
	prev := h.entries
	h.entries = make(map[string]*managedEntry, len(configs))
	h.order = make([]string, 0, len(configs))
	h.mu.Unlock()

	var toClose []Client
	nextEntries := make(map[string]*managedEntry, len(configs))
	nextOrder := make([]string, 0, len(configs))
	statuses := make([]ServerStatus, 0, len(configs))

	seenID := map[string]bool{}
	for _, cfg := range configs {
		if cfg.ID == "" || seenID[cfg.ID] {
			continue
		}
		seenID[cfg.ID] = true
		nextOrder = append(nextOrder, cfg.ID)
		sig := serverConfigSignature(cfg)

		old, hadOld := prev[cfg.ID]
		if hadOld {
			delete(prev, cfg.ID)
		}

		if !cfg.Enabled {
			if hadOld && old.client != nil {
				toClose = append(toClose, old.client)
			}
			st := ServerStatus{
				ID:        cfg.ID,
				Name:      cfg.Name,
				Transport: cfg.Transport,
				Enabled:   false,
				CheckedAt: time.Now(),
			}
			nextEntries[cfg.ID] = &managedEntry{cfg: cfg.Clone(), sig: sig, status: st}
			statuses = append(statuses, st)
			continue
		}

		// Reuse existing healthy connection if config signature is identical
		if hadOld && old.client != nil && old.sig == sig && old.status.Connected {
			nextEntries[cfg.ID] = old
			statuses = append(statuses, old.status)
			continue
		}
		if hadOld && old.client != nil {
			toClose = append(toClose, old.client)
		}

		started := time.Now()
		dialCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		client, err := Dial(dialCtx, cfg, workDir)
		var st ServerStatus
		st.ID = cfg.ID
		st.Name = cfg.Name
		st.Transport = cfg.Transport
		st.Enabled = true
		st.CheckedAt = started

		if err != nil {
			cancel()
			st.Error = err.Error()
			st.LatencyMs = max(1, time.Since(started).Milliseconds())
			nextEntries[cfg.ID] = &managedEntry{cfg: cfg.Clone(), sig: sig, status: st}
			statuses = append(statuses, st)
			continue
		}

		tools, listErr := client.ListTools(dialCtx)
		cancel()
		st.LatencyMs = max(1, time.Since(started).Milliseconds())
		if listErr != nil {
			_ = client.Close()
			st.Error = listErr.Error()
			nextEntries[cfg.ID] = &managedEntry{cfg: cfg.Clone(), sig: sig, status: st}
			statuses = append(statuses, st)
			continue
		}

		st.Connected = true
		st.ServerName, st.ServerVersion = client.ServerInfo()
		st.Tools = tools
		nextEntries[cfg.ID] = &managedEntry{
			cfg:    cfg.Clone(),
			sig:    sig,
			client: client,
			status: st,
		}
		statuses = append(statuses, st)
	}

	for _, orphan := range prev {
		if orphan.client != nil {
			toClose = append(toClose, orphan.client)
		}
	}
	for _, cl := range toClose {
		_ = cl.Close()
	}

	h.mu.Lock()
	h.entries = nextEntries
	h.order = nextOrder
	h.mu.Unlock()

	return statuses
}

// Statuses returns the latest connection and tool discovery snapshot for all configured MCP servers.
func (h *Hub) Statuses() []ServerStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ServerStatus, 0, len(h.order))
	for _, id := range h.order {
		if e, ok := h.entries[id]; ok {
			cp := e.status
			if len(e.status.Tools) > 0 {
				cp.Tools = make([]ToolSpec, len(e.status.Tools))
				copy(cp.Tools, e.status.Tools)
			}
			out = append(out, cp)
		}
	}
	return out
}

// ActiveToolCount returns the total number of live MCP tools currently bridged into the Agent.
func (h *Hub) ActiveToolCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, id := range h.order {
		if e, ok := h.entries[id]; ok && e.status.Connected {
			total += len(e.status.Tools)
		}
	}
	return total
}

// BuildAgentTools converts all connected MCP servers' tools into sky-valley/pi agent.AgentTool instances.
func (h *Hub) BuildAgentTools() []agent.AgentTool {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []agent.AgentTool
	seenNames := map[string]bool{}

	for _, id := range h.order {
		e, ok := h.entries[id]
		if !ok || !e.status.Connected || e.client == nil {
			continue
		}
		srvDisplay := strings.TrimSpace(e.cfg.Name)
		if srvDisplay == "" {
			srvDisplay = e.status.ServerName
		}
		srvSlug := SanitizeToolSegment(srvDisplay)
		client := e.client

		for _, spec := range e.status.Tools {
			toolSlug := SanitizeToolSegment(spec.Name)
			fullName := fmt.Sprintf("mcp__%s__%s", srvSlug, toolSlug)
			if seenNames[fullName] {
				fullName = fmt.Sprintf("mcp__%s_%s__%s", srvSlug, SanitizeToolSegment(id), toolSlug)
			}
			seenNames[fullName] = true

			desc := spec.Description
			if desc == "" {
				desc = fmt.Sprintf("MCP tool %q from server %q", spec.Name, srvDisplay)
			} else {
				desc = fmt.Sprintf("[MCP:%s] %s", srvDisplay, desc)
			}

			origToolName := spec.Name
			schema := convertInputSchema(spec.InputSchema)

			out = append(out, agent.AgentTool{
				Name:        fullName,
				Description: desc,
				Parameters:  schema,
				Label:       fmt.Sprintf("%s · %s", srvDisplay, origToolName),
				Execute: func(ctx context.Context, _ string, params map[string]any, _ agent.ToolUpdateFunc) (agent.AgentToolResult, error) {
					res, err := client.CallTool(ctx, origToolName, params)
					if err != nil {
						return agent.AgentToolResult{}, err
					}
					var content ai.ContentList
					for _, item := range res.Content {
						switch item.Type {
						case "image":
							if item.Data != "" {
								mime := item.MimeType
								if mime == "" {
									mime = "image/png"
								}
								content = append(content, ai.ImageContent{
									Data:     item.Data,
									MimeType: mime,
								})
							}
						default:
							if item.Text != "" {
								content = append(content, ai.TextContent{
									Text: item.Text,
								})
							}
						}
					}
					if len(content) == 0 {
						content = ai.ContentList{ai.TextContent{Text: "(empty MCP result)"}}
					}
					if res.IsError {
						return agent.AgentToolResult{}, errors.New(ai.ContentText(content, "\n"))
					}
					return agent.AgentToolResult{Content: content}, nil
				},
			})
		}
	}
	return out
}

// Close cleanly shuts down all active MCP server subprocesses and HTTP sessions.
func (h *Hub) Close() error {
	h.mu.Lock()
	entries := h.entries
	h.entries = make(map[string]*managedEntry)
	h.order = nil
	h.mu.Unlock()

	var firstErr error
	for _, e := range entries {
		if e.client != nil {
			if err := e.client.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
