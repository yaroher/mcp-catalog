package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/yaroher/mcp-catalog/internal/config"
)

const proxyProtocolVersion = "2024-11-05"
const proxyTimeout = 30 * time.Second

const proxyResourcePrefix = "mcp-catalog://resource/"
const proxyResourceTemplatePrefix = "mcp-catalog://resource-template/"

func nextUniqueName(base string, used map[string]struct{}) string {
	name := strings.TrimSpace(base)
	if name == "" {
		name = "item"
	}
	if _, ok := used[name]; !ok {
		used[name] = struct{}{}
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", name, i)
		if _, ok := used[candidate]; !ok {
			used[candidate] = struct{}{}
			return candidate
		}
	}
}

type mcpSession struct {
	Tools             map[string]toolRoute
	Prompts           map[string]promptRoute
	Resources         map[string]resourceRoute
	ResourceTemplates map[string]resourceRoute
}

type toolRoute struct {
	ServerName string
	ToolName   string
}

type promptRoute struct {
	ServerName string
	PromptName string
}

type resourceRoute struct {
	ServerName   string
	OriginalURI  string
	TemplateMode bool
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type proxiedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type toolsListResult struct {
	Tools []proxiedTool `json:"tools"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (s *Server) handleMCPProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		s.handleMCPDelete(w, r)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req rpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}

	sessionID := strings.TrimSpace(r.Header.Get("MCP-Session-Id"))
	switch req.Method {
	case "initialize":
		s.handleMCPInitialize(w, req)
		return
	case "notifications/initialized":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		w.Header().Set("MCP-Session-Id", sessionID)
		w.WriteHeader(http.StatusNoContent)
		return
	case "tools/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}

		// Use a context-based timeout loop instead of hardcoded 10 iterations.
		// Return immediately once all servers are ready.
		discoveryCtx, discoveryCancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer discoveryCancel()

	pollLoop:
		for {
			allDone := true
			allInfo := s.mgr.GetAllInfo()
			for _, info := range allInfo {
				if info.Config.Enabled && (info.Status == "unchecked" || info.Status == "checking") {
					allDone = false
					break
				}
			}
			if allDone {
				break pollLoop
			}
			select {
			case <-discoveryCtx.Done():
				break pollLoop
			case <-time.After(200 * time.Millisecond):
				// Poll faster
			}
		}

		tools, routes := s.aggregateTools()
		s.updateSessionTools(sessionID, routes)
		s.writeRPCResult(w, req.ID, toolsListResult{Tools: tools}, sessionID)
		return
	case "tools/call":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		var params toolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeRPCError(w, req.ID, -32602, "invalid tools/call params")
			return
		}
		if params.Name == "" {
			s.writeRPCError(w, req.ID, -32602, "tools/call name is required")
			return
		}
		route, ok := s.resolveToolRoute(sessionID, params.Name)
		if !ok {
			s.writeRPCError(w, req.ID, -32601, "tool not found")
			return
		}
		if route.ServerName == "system" {
			res, err := s.invokeSystemTool(route.ToolName, params.Arguments, sessionID)
			if err != nil {
				s.writeRPCError(w, req.ID, -32000, err.Error())
				return
			}
			s.writeRawResult(w, req.ID, res, sessionID)
			return
		}
		result, err := s.callTool(route.ServerName, route.ToolName, params.Arguments)
		if err != nil {
			s.writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		
		s.writeRawResult(w, req.ID, result, sessionID)
		return
	case "prompts/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		items, routes := s.aggregatePrompts()
		s.updateSessionPrompts(sessionID, routes)
		s.writeRPCResult(w, req.ID, map[string]any{"prompts": items}, sessionID)
		return
	case "prompts/get":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		params := make(map[string]any)
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeRPCError(w, req.ID, -32602, "invalid prompts/get params")
			return
		}
		name, _ := params["name"].(string)
		if name == "" {
			s.writeRPCError(w, req.ID, -32602, "prompts/get name is required")
			return
		}
		route, ok := s.resolvePromptRoute(sessionID, name)
		if !ok {
			s.writeRPCError(w, req.ID, -32601, "prompt not found")
			return
		}
		params["name"] = route.PromptName
		result, err := s.forwardPromptGet(route.ServerName, params)
		if err != nil {
			s.writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		s.writeRawResult(w, req.ID, result, sessionID)
		return
	case "resources/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		items, routes := s.aggregateResources()
		s.updateSessionResources(sessionID, routes)
		s.writeRPCResult(w, req.ID, map[string]any{"resources": items}, sessionID)
		return
	case "resources/templates/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		items, routes := s.aggregateResourceTemplates()
		s.updateSessionResourceTemplates(sessionID, routes)
		s.writeRPCResult(w, req.ID, map[string]any{"resourceTemplates": items}, sessionID)
		return
	case "resources/read":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		params := make(map[string]any)
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeRPCError(w, req.ID, -32602, "invalid resources/read params")
			return
		}
		uri, _ := params["uri"].(string)
		if uri == "" {
			s.writeRPCError(w, req.ID, -32602, "resources/read uri is required")
			return
		}
		route, ok := s.resolveResourceRoute(sessionID, uri)
		if !ok {
			s.writeRPCError(w, req.ID, -32601, "resource not found")
			return
		}
		params["uri"] = route.OriginalURI
		result, err := s.forwardResourceRead(route.ServerName, params)
		if err != nil {
			s.writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		s.writeRawResult(w, req.ID, result, sessionID)
		return
	default:
		s.writeRPCError(w, req.ID, -32601, "method not found")
		return
	}
}

func (s *Server) handleMCPInitialize(w http.ResponseWriter, req rpcReq) {
	sessionID, err := newSessionID()
	if err != nil {
		s.writeRPCError(w, req.ID, -32603, "failed to allocate session")
		return
	}
	s.mcpMu.Lock()
	s.mcpState[sessionID] = &mcpSession{
		Tools:             make(map[string]toolRoute),
		Prompts:           make(map[string]promptRoute),
		Resources:         make(map[string]resourceRoute),
		ResourceTemplates: make(map[string]resourceRoute),
	}
	s.mcpMu.Unlock()

	result := map[string]any{
		"protocolVersion": proxyProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": true,
			},
			"prompts": map[string]any{
				"listChanged": true,
			},
			"resources": map[string]any{
				"listChanged": true,
			},
		},
		"serverInfo": map[string]any{
			"name":    "mcp-catalog-proxy",
			"version": "1.0.0",
		},
	}
	s.writeRPCResult(w, req.ID, result, sessionID)
}

func (s *Server) handleMCPDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.Header.Get("MCP-Session-Id"))
	if sessionID == "" {
		http.Error(w, "missing MCP-Session-Id", http.StatusBadRequest)
		return
	}
	s.mcpMu.Lock()
	delete(s.mcpState, sessionID)
	s.mcpMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) hasSession(sessionID string) bool {
	s.mcpMu.RLock()
	defer s.mcpMu.RUnlock()
	_, ok := s.mcpState[sessionID]
	return ok
}

func (s *Server) updateSessionTools(sessionID string, routes map[string]toolRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.Tools = routes
}

func (s *Server) updateSessionPrompts(sessionID string, routes map[string]promptRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.Prompts = routes
}

func (s *Server) updateSessionResources(sessionID string, routes map[string]resourceRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.Resources = routes
}

func (s *Server) updateSessionResourceTemplates(sessionID string, routes map[string]resourceRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.ResourceTemplates = routes
}

func (s *Server) resolveToolRoute(sessionID, tool string) (toolRoute, bool) {
	s.mcpMu.RLock()
	ss, ok := s.mcpState[sessionID]
	s.mcpMu.RUnlock()
	if ok {
		if r, ok := ss.Tools[tool]; ok {
			return r, true
		}
	}

	// Fallback: resolve by name across all enabled servers when list was not called yet.
	allInfo := s.mgr.GetAllInfo()
	for serverName, info := range allInfo {
		if info == nil || !info.Config.Enabled {
			continue
		}
		for _, t := range info.Tools {
			if t.Name == tool {
				return toolRoute{ServerName: serverName, ToolName: t.Name}, true
			}
		}
	}

	if tool == "available_mcp" {
		return toolRoute{ServerName: "system", ToolName: "available_mcp"}, true
	}

	return toolRoute{}, false
}

func (s *Server) resolvePromptRoute(sessionID, name string) (promptRoute, bool) {
	s.mcpMu.RLock()
	ss, ok := s.mcpState[sessionID]
	s.mcpMu.RUnlock()
	if ok {
		if r, ok := ss.Prompts[name]; ok {
			return r, true
		}
	}

	// Fallback: resolve by name across all enabled servers when list was not called yet.
	allInfo := s.mgr.GetAllInfo()
	for serverName, info := range allInfo {
		if info == nil || !info.Config.Enabled {
			continue
		}
		for _, p := range info.Prompts {
			if p.Name == name {
				return promptRoute{ServerName: serverName, PromptName: p.Name}, true
			}
		}
	}
	return promptRoute{}, false
}

func (s *Server) resolveResourceRoute(sessionID, uri string) (resourceRoute, bool) {
	s.mcpMu.RLock()
	ss, ok := s.mcpState[sessionID]
	s.mcpMu.RUnlock()
	if ok {
		if r, ok := ss.Resources[uri]; ok {
			return r, true
		}
		if r, ok := ss.ResourceTemplates[uri]; ok {
			return r, true
		}
	}

	if r, ok := parseProxyResourceURI(uri); ok {
		return r, true
	}
	return resourceRoute{}, false
}

func (s *Server) aggregateTools() ([]proxiedTool, map[string]toolRoute) {
	tools := make([]proxiedTool, 0)
	routes := make(map[string]toolRoute)
	usedNames := make(map[string]struct{})

	// Internal routes for system tools (NOT listed in tools/list)
	routes["available_mcp"] = toolRoute{ServerName: "system", ToolName: "available_mcp"}
	routes["describe_mcp"] = toolRoute{ServerName: "system", ToolName: "describe_mcp"}

	allInfo := s.mgr.GetAllInfo()
	serverNames := make([]string, 0, len(allInfo))
	for n := range allInfo {
		serverNames = append(serverNames, n)
	}
	sort.Strings(serverNames)

	// Count occurrences of tool names
	toolCounts := make(map[string]int)
	for _, info := range allInfo {
		if info == nil || !info.Config.Enabled {
			continue
		}
		for _, t := range info.Tools {
			toolCounts[t.Name]++
		}
	}

	for _, serverName := range serverNames {
		info := allInfo[serverName]
		if info == nil || !info.Config.Enabled {
			continue
		}
		
		for _, t := range info.Tools {
			name := t.Name
			// Only use server prefix if there's a conflict or tool is too generic
			if toolCounts[t.Name] > 1 || strings.Contains(strings.ToLower(t.Name), "navigate") || strings.Contains(strings.ToLower(t.Name), "click") {
				base := strings.TrimSuffix(serverName, "-mcp")
				name = fmt.Sprintf("%s_%s", base, t.Name)
			}
			
			// Ensure absolute uniqueness in the proxy
			name = nextUniqueName(name, usedNames)
			
			tools = append(tools, proxiedTool{
				Name:        name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
			routes[name] = toolRoute{ServerName: serverName, ToolName: t.Name}
		}
	}
	return tools, routes
}

func (s *Server) aggregatePrompts() ([]map[string]any, map[string]promptRoute) {
	items := make([]map[string]any, 0)
	routes := make(map[string]promptRoute)
	usedNames := make(map[string]struct{})

	allInfo := s.mgr.GetAllInfo()
	serverNames := make([]string, 0, len(allInfo))
	for n := range allInfo {
		serverNames = append(serverNames, n)
	}
	sort.Strings(serverNames)

	for _, serverName := range serverNames {
		info := allInfo[serverName]
		if info == nil || !info.Config.Enabled {
			continue
		}
		for _, p := range info.Prompts {
			proxyName := nextUniqueName(p.Name, usedNames)
			items = append(items, map[string]any{
				"name":        proxyName,
				"description": p.Description,
			})
			routes[proxyName] = promptRoute{ServerName: serverName, PromptName: p.Name}
		}
	}
	return items, routes
}

func (s *Server) aggregateResources() ([]map[string]any, map[string]resourceRoute) {
	items := make([]map[string]any, 0)
	routes := make(map[string]resourceRoute)

	allInfo := s.mgr.GetAllInfo()
	serverNames := make([]string, 0, len(allInfo))
	for n := range allInfo {
		serverNames = append(serverNames, n)
	}
	sort.Strings(serverNames)

	for _, serverName := range serverNames {
		info := allInfo[serverName]
		if info == nil || !info.Config.Enabled {
			continue
		}
		for _, r := range info.Resources {
			proxyURI := buildProxyResourceURI(serverName, r.URI, false)
			items = append(items, map[string]any{
				"uri":         proxyURI,
				"name":        r.Name,
				"description": r.Description,
				"mimeType":    r.MimeType,
			})
			routes[proxyURI] = resourceRoute{ServerName: serverName, OriginalURI: r.URI}
		}
	}
	return items, routes
}

func (s *Server) aggregateResourceTemplates() ([]map[string]any, map[string]resourceRoute) {
	items := make([]map[string]any, 0)
	routes := make(map[string]resourceRoute)

	allInfo := s.mgr.GetAllInfo()
	for _, info := range allInfo {
		if info == nil || !info.Config.Enabled {
			continue
		}
		// NOTE: ServerInfo doesn't currently store ResourceTemplates explicitly in manager.go
		// but we can add them if needed. For now, we only have Tools, Prompts, Resources.
	}
	return items, routes
}

func (s *Server) listTools(serverName string, srv *config.MCPServer) ([]proxiedTool, error) {
	res, err := s.forwardMCP(serverName, srv, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Tools []proxiedTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return nil, err
	}
	return parsed.Tools, nil
}

func (s *Server) callTool(serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
	srv, ok := s.store.GetServer(serverName)
	if !ok {
		return nil, fmt.Errorf("server %q not found", serverName)
	}

	var parsedArgs any = map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsedArgs); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	params := map[string]any{
		"name":      toolName,
		"arguments": parsedArgs,
	}
	return s.forwardMCP(serverName, srv, "tools/call", params)
}

func (s *Server) forwardPromptGet(serverName string, params map[string]any) (json.RawMessage, error) {
	srv, ok := s.store.GetServer(serverName)
	if !ok {
		return nil, fmt.Errorf("server %q not found", serverName)
	}
	return s.forwardMCP(serverName, srv, "prompts/get", params)
}

func (s *Server) forwardResourceRead(serverName string, params map[string]any) (json.RawMessage, error) {
	srv, ok := s.store.GetServer(serverName)
	if !ok {
		return nil, fmt.Errorf("server %q not found", serverName)
	}
	return s.forwardMCP(serverName, srv, "resources/read", params)
}

func (s *Server) forwardMCP(serverName string, srv *config.MCPServer, method string, params any) (json.RawMessage, error) {
	_ = serverName
	ctx, cancel := context.WithTimeout(context.Background(), proxyTimeout)
	defer cancel()
	if strings.EqualFold(strings.TrimSpace(srv.Type), "streamableHttp") || (strings.TrimSpace(srv.URL) != "" && strings.TrimSpace(srv.Command) == "") {
		return forwardHTTP(ctx, srv, method, params)
	}
	return forwardStdio(ctx, srv, method, params)
}

func forwardHTTP(ctx context.Context, srv *config.MCPServer, method string, params any) (json.RawMessage, error) {
	url := strings.TrimSpace(srv.URL)
	if url == "" {
		return nil, fmt.Errorf("missing url")
	}
	client := &http.Client{Timeout: proxyTimeout}
	sessionID := ""

	send := func(payload map[string]any, expect bool, expectedID int) (*rpcResp, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("MCP-Session-Id", sessionID)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if sid := strings.TrimSpace(resp.Header.Get("MCP-Session-Id")); sid != "" {
			sessionID = sid
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		if !expect {
			return nil, nil
		}
		return decodeProxyResponse(raw, expectedID)
	}

	closeSession := func() {
		if sessionID == "" {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("MCP-Session-Id", sessionID)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	defer closeSession()

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": proxyProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-catalog-proxy",
				"version": "1.0.0",
			},
		},
	}
	initResp, err := send(initReq, true, 1)
	if err != nil {
		return nil, fmt.Errorf("initialize request: %w", err)
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	if _, err := send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}, false, 0); err != nil {
		// non-fatal
	}

	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  method,
		"params":  params,
	}
	callResp, err := send(callReq, true, 2)
	if err != nil {
		return nil, err
	}
	if callResp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, callResp.Error.Message)
	}
	return callResp.Result, nil
}

func forwardStdio(ctx context.Context, srv *config.MCPServer, method string, params any) (json.RawMessage, error) {
	command := strings.TrimSpace(srv.Command)
	if command == "" {
		return nil, fmt.Errorf("missing command")
	}
	cmd := exec.CommandContext(ctx, command, srv.Args...)
	if len(srv.Env) > 0 {
		env := cmd.Environ()
		for k, v := range srv.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	go io.Copy(io.Discard, stderrPipe)

	stdout := bufio.NewReader(stdoutPipe)
	writeReq := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}
	readResp := func() (*rpcResp, error) {
		line, err := stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var resp rpcResp
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}

	if err := writeReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": proxyProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-catalog-proxy",
				"version": "1.0.0",
			},
		},
	}); err != nil {
		return nil, err
	}
	initResp, err := readResp()
	if err != nil {
		return nil, err
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	_ = writeReq(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	if err := writeReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}
	callResp, err := readResp()
	if err != nil {
		return nil, err
	}
	if callResp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, callResp.Error.Message)
	}

	if len(callResp.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return callResp.Result, nil
}

func decodeProxyResponse(raw []byte, expectedID int) (*rpcResp, error) {
	data := strings.TrimSpace(string(raw))
	if data == "" {
		return nil, fmt.Errorf("empty response body")
	}
	var candidates []rpcResp
	add := func(v rpcResp) {
		if v.JSONRPC == "" && v.Result == nil && v.Error == nil {
			return
		}
		candidates = append(candidates, v)
	}

	var one rpcResp
	if err := json.Unmarshal([]byte(data), &one); err == nil {
		add(one)
	}
	var arr []rpcResp
	if err := json.Unmarshal([]byte(data), &arr); err == nil {
		for _, v := range arr {
			add(v)
		}
	}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var sseOne rpcResp
		if err := json.Unmarshal([]byte(payload), &sseOne); err == nil {
			add(sseOne)
			continue
		}
		var sseArr []rpcResp
		if err := json.Unmarshal([]byte(payload), &sseArr); err == nil {
			for _, v := range sseArr {
				add(v)
			}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("unable to decode response: %s", data)
	}
	if expectedID > 0 {
		for i := range candidates {
			if candidates[i].ID == expectedID {
				return &candidates[i], nil
			}
		}
		return nil, fmt.Errorf("response id=%d not found", expectedID)
	}
	return &candidates[0], nil
}

func parseListObjects(raw json.RawMessage, key string) ([]map[string]any, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	listRaw, ok := payload[key]
	if !ok {
		return []map[string]any{}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(listRaw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func buildProxyResourceURI(serverName, originalURI string, template bool) string {
	encoded := hex.EncodeToString([]byte(serverName + "\x1f" + originalURI))
	if template {
		return proxyResourceTemplatePrefix + encoded
	}
	return proxyResourcePrefix + encoded
}

func parseProxyResourceURI(uri string) (resourceRoute, bool) {
	prefix := proxyResourcePrefix
	template := false
	if strings.HasPrefix(uri, proxyResourceTemplatePrefix) {
		prefix = proxyResourceTemplatePrefix
		template = true
	} else if !strings.HasPrefix(uri, proxyResourcePrefix) {
		return resourceRoute{}, false
	}
	value := strings.TrimPrefix(uri, prefix)
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return resourceRoute{}, false
	}
	payload := string(decoded)
	parts := strings.SplitN(payload, "\x1f", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return resourceRoute{}, false
	}
	return resourceRoute{ServerName: parts[0], OriginalURI: parts[1], TemplateMode: template}, true
}

func (s *Server) invokeSystemTool(toolName string, args json.RawMessage, sessionID string) (json.RawMessage, error) {
	switch toolName {
	case "available_mcp":
		allInfo := s.mgr.GetAllInfo()
		type statusEntry struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			ToolsCount int    `json:"toolsCount"`
		}
		var result []statusEntry
		for name, info := range allInfo {
			if !info.Config.Enabled {
				continue
			}
			result = append(result, statusEntry{
				Name:       name,
				Status:     string(info.Status),
				ToolsCount: len(info.Tools),
			})
		}

		b, _ := json.MarshalIndent(result, "", "  ")
		content := map[string]any{
			"content": []any{
				map[string]any{
					"type": "text",
					"text": fmt.Sprintf("Downstream MCP Servers Status:\n%s\n\nAll tools from these servers are integrated directly into this proxy. You can see them in 'tools/list'.", string(b)),
				},
			},
		}
		return json.Marshal(content)

	case "describe_mcp":
		var params struct {
			ServerName string `json:"serverName"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("invalid describe_mcp params")
		}
		params.ServerName = strings.TrimSpace(params.ServerName)

		info, ok := s.mgr.GetInfo(params.ServerName)
		if !ok {
			return nil, fmt.Errorf("server %q not found", params.ServerName)
		}

		// Ensure we have tools
		if info.Status == "unchecked" || info.Status == "checking" {
			if info.Status == "unchecked" {
				_ = s.mgr.Check(params.ServerName)
			} else {
				for i := 0; i < 20; i++ {
					time.Sleep(500 * time.Millisecond)
					info, _ = s.mgr.GetInfo(params.ServerName)
					if info.Status != "checking" {
						break
					}
				}
			}
			info, _ = s.mgr.GetInfo(params.ServerName)
		}

		type toolMapping struct {
			ProxiedName string `json:"proxiedName"`
			Original    string `json:"original"`
			Description string `json:"description,omitempty"`
		}
		tools := make([]toolMapping, 0)

		if info.Status == "healthy" {
			allInfo := s.mgr.GetAllInfo()
			serverNames := make([]string, 0, len(allInfo))
			for n := range allInfo {
				serverNames = append(serverNames, n)
			}
			sort.Strings(serverNames)

			usedNames := make(map[string]struct{})
			usedNames["available_mcp"] = struct{}{}
			usedNames["describe_mcp"] = struct{}{}

			for _, sName := range serverNames {
				sInfo := allInfo[sName]
				if sInfo == nil || !sInfo.Config.Enabled {
					continue
				}
				for _, t := range sInfo.Tools {
					pName := nextUniqueName(t.Name, usedNames)
					if strings.EqualFold(sName, params.ServerName) {
						tools = append(tools, toolMapping{
							ProxiedName: pName,
							Original:    t.Name,
							Description: t.Description,
						})
					}
				}
			}
		}

		header := fmt.Sprintf("SERVER: %s\nSTATUS: %s\n", params.ServerName, info.Status)
		if info.Error != "" {
			header += fmt.Sprintf("ERROR: %s\n", info.Error)
			header += "\nFIX REQUIRED: The server is down. Do not try to call its tools.\n"
		}

		instruction := "\nNEXT STEP FOR AGENT: Find the 'proxiedName' for your task in the list below. Then, immediately execute it using tools/call on THIS proxy server. Do NOT search for resources or list_mcp_resources. CALL THE TOOL DIRECTLY.\n"

		verificationHint := ""
		if strings.Contains(strings.ToLower(params.ServerName), "chrome") {
			verificationHint = "\nVERIFICATION HINT: After your action (e.g. navigation), confirm with 'get_windows_and_tabs'.\n"
		}

		footer := ""
		if info.Status == "healthy" && len(tools) == 0 {
			footer = "\nWARNING: No tools found for this server."
		}

		b, _ := json.MarshalIndent(tools, "", "  ")
		content := map[string]any{
			"content": []any{
				map[string]any{
					"type": "text",
					"text": fmt.Sprintf("%s%s%s\nTOOLS MAPPING (CALL THESE NAMES DIRECTLY):\n%s%s", header, instruction, verificationHint, string(b), footer),
				},
			},
		}
		return json.Marshal(content)

	default:
		return nil, fmt.Errorf("system tool %q not found", toolName)
	}
}

func (s *Server) enhanceToolResult(result json.RawMessage, serverName string) json.RawMessage {
	if len(result) == 0 {
		return result
	}
	var parsed struct {
		Content []map[string]any `json:"content"`
		IsError bool             `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		// Not a standard MCP tool result, return as is
		return result
	}

	// Add a clear success indicator for agents
	msg := fmt.Sprintf("[Proxy] Tool successfully executed on downstream server %q.", serverName)
	parsed.Content = append(parsed.Content, map[string]any{
		"type": "text",
		"text": msg,
	})

	enhanced, err := json.Marshal(parsed)
	if err != nil {
		return result
	}
	return enhanced
}

func (s *Server) writeRPCResult(w http.ResponseWriter, id int, result any, sessionID string) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.writeRPCError(w, id, -32603, "failed to encode result")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if sessionID != "" {
		w.Header().Set("MCP-Session-Id", sessionID)
	}
	_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Result: raw})
}

func (s *Server) writeRawResult(w http.ResponseWriter, id int, result json.RawMessage, sessionID string) {
	w.Header().Set("Content-Type", "application/json")
	if sessionID != "" {
		w.Header().Set("MCP-Session-Id", sessionID)
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeRPCError(w http.ResponseWriter, id int, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}})
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
