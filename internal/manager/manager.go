package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/yaroher/mcp-catalog/internal/config"
)

type ServerStatus string

const (
	StatusUnchecked ServerStatus = "unchecked"
	StatusChecking  ServerStatus = "checking"
	StatusHealthy   ServerStatus = "healthy"
	StatusError     ServerStatus = "error"
)

type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type ServerInfo struct {
	Name            string           `json:"name"`
	Virtual         bool             `json:"virtual,omitempty"`
	Config          config.MCPServer `json:"config"`
	Status          ServerStatus     `json:"status"`
	Error           string           `json:"error,omitempty"`
	Logs            []LogEntry       `json:"logs"`
	Tools           []MCPTool        `json:"tools"`
	Prompts         []MCPPrompt      `json:"prompts"`
	Resources       []MCPResource    `json:"resources"`
	LastCheck       *time.Time       `json:"lastCheck,omitempty"`
	ServerName      string           `json:"serverName,omitempty"`
	ServerVersion   string           `json:"serverVersion,omitempty"`
	ProtocolVersion string           `json:"protocolVersion,omitempty"`
	CheckDuration   int64            `json:"checkDuration,omitempty"`
}

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type mcpToolsResult struct {
	Tools []MCPTool `json:"tools"`
}

type MCPPrompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type mcpPromptsResult struct {
	Prompts []MCPPrompt `json:"prompts"`
}

type MCPResource struct {
	Name        string `json:"name,omitempty"`
	URI         string `json:"uri"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type mcpResourcesResult struct {
	Resources []MCPResource `json:"resources"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpInitResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	ServerInfo      mcpServerInfoResp `json:"serverInfo"`
}

type mcpServerInfoResp struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

const maxLogEntries = 500
const checkTimeout = 30 * time.Second

type Manager struct {
	store          *config.Store
	servers        map[string]*ServerInfo
	mu             sync.RWMutex
	listeners      []func(name string, info *ServerInfo)
	listMu         sync.RWMutex
	healthInterval int
	healthMu       sync.RWMutex
	stopHealth     chan struct{}
}

func New(store *config.Store) *Manager {
	return &Manager{
		store:          store,
		servers:        make(map[string]*ServerInfo),
		healthInterval: store.GetHealthCheckInterval(),
		stopHealth:     make(chan struct{}),
	}
}

func (m *Manager) GetHealthInterval() int {
	m.healthMu.RLock()
	defer m.healthMu.RUnlock()
	return m.healthInterval
}

func (m *Manager) SetHealthInterval(seconds int) {
	m.healthMu.Lock()
	m.healthInterval = seconds
	m.healthMu.Unlock()
}

func (m *Manager) OnChange(fn func(name string, info *ServerInfo)) {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	m.listeners = append(m.listeners, fn)
}

func (m *Manager) notify(name string, info *ServerInfo) {
	m.listMu.RLock()
	defer m.listMu.RUnlock()
	for _, fn := range m.listeners {
		go fn(name, info)
	}
}

func (m *Manager) getOrCreateInfo(name string) *ServerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	if info, ok := m.servers[name]; ok {
		return info
	}

	srv, ok := m.store.GetServer(name)
	if !ok {
		return nil
	}

	info := &ServerInfo{
		Name:      name,
		Config:    *srv,
		Status:    StatusUnchecked,
		Logs:      make([]LogEntry, 0),
		Tools:     make([]MCPTool, 0),
		Prompts:   make([]MCPPrompt, 0),
		Resources: make([]MCPResource, 0),
	}
	m.servers[name] = info
	return info
}

func (m *Manager) addLog(info *ServerInfo, level, msg string) {
	entry := LogEntry{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}
	info.Logs = append(info.Logs, entry)
	if len(info.Logs) > maxLogEntries {
		info.Logs = info.Logs[len(info.Logs)-maxLogEntries:]
	}
}

// Check starts the server temporarily, verifies MCP initialize works, discovers tools, then stops it.
func (m *Manager) Check(name string) error {
	srv, ok := m.store.GetServer(name)
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}

	info := m.getOrCreateInfo(name)
	if info == nil {
		return fmt.Errorf("server %q not found", name)
	}

	// Mark as checking
	m.mu.Lock()
	info.Status = StatusChecking
	info.Error = ""
	info.Config = *srv
	m.mu.Unlock()
	target := strings.TrimSpace(strings.Join(append([]string{srv.Command}, srv.Args...), " "))
	if isStreamableHTTPServer(srv) {
		target = fmt.Sprintf("streamableHttp %s", srv.URL)
	}
	if target == "" {
		target = "(invalid config: no command/url)"
	}
	m.addLog(info, "info", fmt.Sprintf("Checking: %s", target))
	m.notify(name, info)

	// Run the actual check
	err := m.doCheck(name, srv, info)

	now := time.Now()
	m.mu.Lock()
	info.LastCheck = &now
	if err != nil {
		info.Status = StatusError
		info.Error = err.Error()
	} else {
		info.Status = StatusHealthy
		info.Error = ""
	}
	m.mu.Unlock()
	m.notify(name, info)

	return err
}

func (m *Manager) doCheck(name string, srv *config.MCPServer, info *ServerInfo) error {
	_ = name
	if isStreamableHTTPServer(srv) {
		return m.doCheckStreamableHTTP(srv, info)
	}
	if srv.Command == "" {
		err := fmt.Errorf("missing command for stdio server")
		m.addLog(info, "error", err.Error())
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)

	if len(srv.Env) > 0 {
		env := cmd.Environ()
		for k, v := range srv.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.addLog(info, "error", fmt.Sprintf("stdin pipe: %v", err))
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		m.addLog(info, "error", fmt.Sprintf("stdout pipe: %v", err))
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		m.addLog(info, "error", fmt.Sprintf("stderr pipe: %v", err))
		return fmt.Errorf("stderr pipe: %w", err)
	}

	startTime := time.Now()

	if err := cmd.Start(); err != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Failed to start: %v", err))
		return fmt.Errorf("start: %w", err)
	}
	m.addLog(info, "info", fmt.Sprintf("Started with PID %d", cmd.Process.Pid))

	// Collect stderr in background
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			m.addLog(info, "stderr", scanner.Text())
		}
	}()

	stdout := bufio.NewReader(stdoutPipe)

	// Send MCP initialize
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-manager","version":"1.0.0"}}}` + "\n"
	if _, err := stdin.Write([]byte(initReq)); err != nil {
		cancel()
		m.addLog(info, "error", fmt.Sprintf("Failed to send initialize: %v", err))
		return fmt.Errorf("send initialize: %w", err)
	}

	// Read initialize response
	line, err := stdout.ReadString('\n')
	if err != nil {
		cancel()
		m.addLog(info, "error", fmt.Sprintf("Failed to read initialize response: %v", err))
		return fmt.Errorf("read initialize response: %w", err)
	}

	var initResp mcpResponse
	if err := json.Unmarshal([]byte(line), &initResp); err != nil {
		cancel()
		m.addLog(info, "error", fmt.Sprintf("Invalid initialize response: %v", err))
		return fmt.Errorf("parse initialize response: %w", err)
	}

	if initResp.Error != nil {
		cancel()
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Initialize error: %s", initResp.Error.Message))
		return fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	// Extract server info from initialize result
	var initResult mcpInitResult
	if err := json.Unmarshal(initResp.Result, &initResult); err == nil {
		info.ServerName = initResult.ServerInfo.Name
		info.ServerVersion = initResult.ServerInfo.Version
		info.ProtocolVersion = initResult.ProtocolVersion
	}

	m.addLog(info, "info", fmt.Sprintf("MCP initialized: %s %s (protocol %s)",
		info.ServerName, info.ServerVersion, info.ProtocolVersion))

	// Send initialized notification
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	stdin.Write([]byte(notif))

	// List tools
	toolsReq := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"
	if _, err := stdin.Write([]byte(toolsReq)); err != nil {
		cancel()
		m.addLog(info, "warn", fmt.Sprintf("Failed to send tools/list: %v", err))
		// Not a fatal error — initialize succeeded
		return nil
	}

	line, err = stdout.ReadString('\n')
	if err != nil {
		cancel()
		m.addLog(info, "warn", fmt.Sprintf("Failed to read tools/list response: %v", err))
		return nil
	}

	var toolsResp mcpResponse
	if err := json.Unmarshal([]byte(line), &toolsResp); err != nil {
		m.addLog(info, "warn", fmt.Sprintf("Invalid tools/list response: %v", err))
	} else if toolsResp.Error != nil {
		m.addLog(info, "warn", fmt.Sprintf("tools/list error: %s", toolsResp.Error.Message))
	} else {
		var result mcpToolsResult
		if err := json.Unmarshal(toolsResp.Result, &result); err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to parse tools: %v", err))
		} else {
			m.mu.Lock()
			info.Tools = result.Tools
			m.mu.Unlock()
			m.addLog(info, "info", fmt.Sprintf("Discovered %d tools", len(result.Tools)))
		}
	}

	// List prompts
	promptsReq := `{"jsonrpc":"2.0","id":3,"method":"prompts/list","params":{}}` + "\n"
	if _, err := stdin.Write([]byte(promptsReq)); err != nil {
		m.addLog(info, "warn", fmt.Sprintf("Failed to send prompts/list: %v", err))
	} else {
		line, err = stdout.ReadString('\n')
		if err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to read prompts/list response: %v", err))
		} else {
			var promptsResp mcpResponse
			if err := json.Unmarshal([]byte(line), &promptsResp); err != nil {
				m.addLog(info, "warn", fmt.Sprintf("Invalid prompts/list response: %v", err))
			} else if promptsResp.Error != nil {
				m.addLog(info, "warn", fmt.Sprintf("prompts/list error: %s", promptsResp.Error.Message))
			} else {
				var result mcpPromptsResult
				if err := json.Unmarshal(promptsResp.Result, &result); err != nil {
					m.addLog(info, "warn", fmt.Sprintf("Failed to parse prompts: %v", err))
				} else {
					m.mu.Lock()
					info.Prompts = result.Prompts
					m.mu.Unlock()
					m.addLog(info, "info", fmt.Sprintf("Discovered %d prompts", len(result.Prompts)))
				}
			}
		}
	}

	// List resources
	resourcesReq := `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}` + "\n"
	if _, err := stdin.Write([]byte(resourcesReq)); err != nil {
		m.addLog(info, "warn", fmt.Sprintf("Failed to send resources/list: %v", err))
	} else {
		line, err = stdout.ReadString('\n')
		if err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to read resources/list response: %v", err))
		} else {
			var resourcesResp mcpResponse
			if err := json.Unmarshal([]byte(line), &resourcesResp); err != nil {
				m.addLog(info, "warn", fmt.Sprintf("Invalid resources/list response: %v", err))
			} else if resourcesResp.Error != nil {
				m.addLog(info, "warn", fmt.Sprintf("resources/list error: %s", resourcesResp.Error.Message))
			} else {
				var result mcpResourcesResult
				if err := json.Unmarshal(resourcesResp.Result, &result); err != nil {
					m.addLog(info, "warn", fmt.Sprintf("Failed to parse resources: %v", err))
				} else {
					m.mu.Lock()
					info.Resources = result.Resources
					m.mu.Unlock()
					m.addLog(info, "info", fmt.Sprintf("Discovered %d resources", len(result.Resources)))
				}
			}
		}
	}

	// Kill the process
	cancel()
	cmd.Wait()
	<-stderrDone

	info.CheckDuration = time.Since(startTime).Milliseconds()
	m.addLog(info, "info", fmt.Sprintf("Check completed in %dms, process stopped", info.CheckDuration))

	return nil
}

func isStreamableHTTPServer(srv *config.MCPServer) bool {
	if srv == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(srv.Type), "streamableHttp") {
		return true
	}
	return strings.TrimSpace(srv.URL) != "" && strings.TrimSpace(srv.Command) == ""
}

func (m *Manager) doCheckStreamableHTTP(srv *config.MCPServer, info *ServerInfo) error {
	if srv.URL == "" {
		err := fmt.Errorf("missing url for streamableHttp server")
		m.addLog(info, "error", err.Error())
		return err
	}

	startTime := time.Now()
	m.addLog(info, "info", fmt.Sprintf("Connecting via streamable HTTP: %s", srv.URL))
	client := &http.Client{Timeout: checkTimeout}
	sessionID := ""
	defer func() {
		if sessionID != "" {
			if err := closeStreamableHTTPSession(client, srv.URL, sessionID); err != nil {
				m.addLog(info, "warn", fmt.Sprintf("Failed to close HTTP MCP session %q: %v", sessionID, err))
			}
		}
	}()

	send := func(payload map[string]any, expectResponse bool, expectedID int) (*mcpResponse, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}

		req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("MCP-Session-Id", sessionID)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
		defer resp.Body.Close()
		if id := strings.TrimSpace(resp.Header.Get("MCP-Session-Id")); id != "" {
			sessionID = id
		}

		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}

		if !expectResponse {
			io.Copy(io.Discard, resp.Body)
			return nil, nil
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		parsed, err := decodeHTTPMCPResponse(raw, expectedID)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	}

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-manager",
				"version": "1.0.0",
			},
		},
	}

	initResp, err := send(initReq, true, 1)
	if err != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Initialize request failed: %v", err))
		return fmt.Errorf("initialize request: %w", err)
	}

	if initResp.Error != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Initialize error: %s", initResp.Error.Message))
		return fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	var initResult mcpInitResult
	if err := json.Unmarshal(initResp.Result, &initResult); err == nil {
		info.ServerName = initResult.ServerInfo.Name
		info.ServerVersion = initResult.ServerInfo.Version
		info.ProtocolVersion = initResult.ProtocolVersion
	}
	m.addLog(info, "info", fmt.Sprintf("MCP initialized: %s %s (protocol %s)",
		info.ServerName, info.ServerVersion, info.ProtocolVersion))

	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	if _, err := send(notif, false, 0); err != nil {
		m.addLog(info, "warn", fmt.Sprintf("Failed to send initialized notification: %v", err))
	}

	toolsReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	toolsResp, err := send(toolsReq, true, 2)
	if err != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "warn", fmt.Sprintf("tools/list request failed: %v", err))
		return nil
	}

	if toolsResp.Error != nil {
		m.addLog(info, "warn", fmt.Sprintf("tools/list error: %s", toolsResp.Error.Message))
	} else {
		var result mcpToolsResult
		if err := json.Unmarshal(toolsResp.Result, &result); err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to parse tools: %v", err))
		} else {
			m.mu.Lock()
			info.Tools = result.Tools
			m.mu.Unlock()
			m.addLog(info, "info", fmt.Sprintf("Discovered %d tools", len(result.Tools)))
		}
	}

	promptsReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "prompts/list",
		"params":  map[string]any{},
	}
	promptsResp, err := send(promptsReq, true, 3)
	if err != nil {
		m.addLog(info, "warn", fmt.Sprintf("prompts/list request failed: %v", err))
	} else if promptsResp.Error != nil {
		m.addLog(info, "warn", fmt.Sprintf("prompts/list error: %s", promptsResp.Error.Message))
	} else {
		var result mcpPromptsResult
		if err := json.Unmarshal(promptsResp.Result, &result); err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to parse prompts: %v", err))
		} else {
			m.mu.Lock()
			info.Prompts = result.Prompts
			m.mu.Unlock()
			m.addLog(info, "info", fmt.Sprintf("Discovered %d prompts", len(result.Prompts)))
		}
	}

	resourcesReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "resources/list",
		"params":  map[string]any{},
	}
	resourcesResp, err := send(resourcesReq, true, 4)
	if err != nil {
		m.addLog(info, "warn", fmt.Sprintf("resources/list request failed: %v", err))
	} else if resourcesResp.Error != nil {
		m.addLog(info, "warn", fmt.Sprintf("resources/list error: %s", resourcesResp.Error.Message))
	} else {
		var result mcpResourcesResult
		if err := json.Unmarshal(resourcesResp.Result, &result); err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to parse resources: %v", err))
		} else {
			m.mu.Lock()
			info.Resources = result.Resources
			m.mu.Unlock()
			m.addLog(info, "info", fmt.Sprintf("Discovered %d resources", len(result.Resources)))
		}
	}

	info.CheckDuration = time.Since(startTime).Milliseconds()
	m.addLog(info, "info", fmt.Sprintf("Check completed in %dms", info.CheckDuration))
	return nil
}

func decodeHTTPMCPResponse(raw []byte, expectedID int) (*mcpResponse, error) {
	data := strings.TrimSpace(string(raw))
	if data == "" {
		return nil, fmt.Errorf("empty response body")
	}

	var candidates []mcpResponse
	addCandidate := func(resp mcpResponse) {
		if resp.JSONRPC == "" && resp.Result == nil && resp.Error == nil {
			return
		}
		candidates = append(candidates, resp)
	}

	var single mcpResponse
	if err := json.Unmarshal([]byte(data), &single); err == nil {
		addCandidate(single)
	}

	var batch []mcpResponse
	if err := json.Unmarshal([]byte(data), &batch); err == nil && len(batch) > 0 {
		for _, resp := range batch {
			addCandidate(resp)
		}
	}

	// Fallback for SSE replies where payload comes as "data: {json}" lines.
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var sseSingle mcpResponse
		if err := json.Unmarshal([]byte(payload), &sseSingle); err == nil {
			addCandidate(sseSingle)
			continue
		}

		var sseBatch []mcpResponse
		if err := json.Unmarshal([]byte(payload), &sseBatch); err == nil {
			for _, resp := range sseBatch {
				addCandidate(resp)
			}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("unable to decode MCP response: %s", data)
	}

	if expectedID > 0 {
		for i := range candidates {
			if candidates[i].ID == expectedID {
				return &candidates[i], nil
			}
		}
		return nil, fmt.Errorf("response for id=%d not found in body: %s", expectedID, data)
	}

	return &candidates[0], nil
}

func closeStreamableHTTPSession(client *http.Client, url, sessionID string) error {
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("create close request: %w", err)
	}
	req.Header.Set("MCP-Session-Id", sessionID)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send close request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("close status %d", resp.StatusCode)
	}
	return nil
}

// CheckAll checks all enabled servers in parallel.
func (m *Manager) CheckAll() {
	cfg := m.store.Get()
	var wg sync.WaitGroup
	for name, srv := range cfg.MCPServers {
		if srv.Enabled {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				m.Check(n)
			}(name)
		}
	}
	wg.Wait()
}

// StartHealthLoop runs periodic health checks in background.
func (m *Manager) StartHealthLoop() {
	for {
		m.healthMu.RLock()
		interval := m.healthInterval
		m.healthMu.RUnlock()

		if interval <= 0 {
			// Disabled, poll every 5s to see if it gets enabled
			select {
			case <-m.stopHealth:
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		select {
		case <-m.stopHealth:
			return
		case <-time.After(time.Duration(interval) * time.Second):
			m.CheckAll()
		}
	}
}

// StopHealthLoop stops the background health loop.
func (m *Manager) StopHealthLoop() {
	close(m.stopHealth)
}

// RemoveServer removes cached info for a deleted server.
func (m *Manager) RemoveServer(name string) {
	m.mu.Lock()
	delete(m.servers, name)
	m.mu.Unlock()
}

func (m *Manager) GetInfo(name string) (*ServerInfo, bool) {
	m.mu.RLock()
	info, ok := m.servers[name]
	m.mu.RUnlock()
	if !ok {
		srv, ok := m.store.GetServer(name)
		if !ok {
			return nil, false
		}
		return &ServerInfo{
			Name:      name,
			Config:    *srv,
			Status:    StatusUnchecked,
			Logs:      []LogEntry{},
			Tools:     []MCPTool{},
			Prompts:   []MCPPrompt{},
			Resources: []MCPResource{},
		}, true
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	// Return a copy
	cp := *info
	cp.Logs = make([]LogEntry, len(info.Logs))
	copy(cp.Logs, info.Logs)
	cp.Tools = make([]MCPTool, len(info.Tools))
	copy(cp.Tools, info.Tools)
	cp.Prompts = make([]MCPPrompt, len(info.Prompts))
	copy(cp.Prompts, info.Prompts)
	cp.Resources = make([]MCPResource, len(info.Resources))
	copy(cp.Resources, info.Resources)
	return &cp, true
}

func (m *Manager) GetAllInfo() map[string]*ServerInfo {
	cfg := m.store.Get()
	result := make(map[string]*ServerInfo)
	for name := range cfg.MCPServers {
		info, ok := m.GetInfo(name)
		if ok {
			result[name] = info
		}
	}
	return result
}
