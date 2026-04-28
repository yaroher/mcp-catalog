package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/yaroher/mcp-catalog/internal/config"
	"github.com/yaroher/mcp-catalog/internal/manager"
)

// RunMCPStdio starts the MCP proxy transport over stdio.
func RunMCPStdio(store *config.Store) error {
	mgr := manager.New(store)
	// Start discovery and wait for it to complete
	mgr.CheckAll()
	go mgr.StartHealthLoop()

	s := &Server{
		store: store,
		mgr:   mgr,
	}
	return s.runMCPStdio()
}

func (s *Server) runMCPStdio() error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 2*1024*1024)
	out := bufio.NewWriter(os.Stdout)

	toolRoutes := make(map[string]toolRoute)
	promptRoutes := make(map[string]promptRoute)
	resourceRoutes := make(map[string]resourceRoute)
	templateRoutes := make(map[string]resourceRoute)

	write := func(resp rpcResp) error {
		b, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if _, err := out.Write(append(b, '\n')); err != nil {
			return err
		}
		return out.Flush()
	}

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = write(rpcResp{JSONRPC: "2.0", ID: 0, Error: &rpcErr{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.JSONRPC == "" {
			req.JSONRPC = "2.0"
		}

		switch req.Method {
		case "initialize":
			raw, _ := json.Marshal(map[string]any{
				"protocolVersion": proxyProtocolVersion,
				"capabilities": map[string]any{
					"tools":     map[string]any{"listChanged": true},
					"prompts":   map[string]any{"listChanged": true},
					"resources": map[string]any{"listChanged": true},
				},
				"serverInfo": map[string]any{
					"name":    "mcp-catalog-proxy",
					"version": "1.0.0",
				},
			})
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: raw})
		case "notifications/initialized":
			// notifications have no response
		case "tools/list":
			tools, routes := s.aggregateTools()
			toolRoutes = routes
			raw, _ := json.Marshal(toolsListResult{Tools: tools})
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: raw})
		case "tools/call":
			var p toolsCallParams
			if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: "invalid tools/call params"}})
				continue
			}
			route, ok := toolRoutes[p.Name]
			if !ok {
				route, ok = s.resolveToolRoute("", p.Name)
				if !ok {
					_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: "tool not found"}})
					continue
				}
			}
			var res json.RawMessage
			var err error
			if route.ServerName == "system" {
				res, err = s.invokeSystemTool(route.ToolName, p.Arguments, "")
			} else {
				res, err = s.callTool(route.ServerName, route.ToolName, p.Arguments)
				if err == nil {
					res = s.enhanceToolResult(res, route.ServerName)
				}
			}
			if err != nil {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32000, Message: err.Error()}})
				continue
			}
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: res})
		case "prompts/list":
			items, routes := s.aggregatePrompts()
			promptRoutes = routes
			raw, _ := json.Marshal(map[string]any{"prompts": items})
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: raw})
		case "prompts/get":
			params := map[string]any{}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: "invalid prompts/get params"}})
				continue
			}
			name, _ := params["name"].(string)
			route, ok := promptRoutes[name]
			if !ok {
				route, ok = s.resolvePromptRoute("", name)
			}
			if !ok {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: "prompt not found"}})
				continue
			}
			params["name"] = route.PromptName
			res, err := s.forwardPromptGet(route.ServerName, params)
			if err != nil {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32000, Message: err.Error()}})
				continue
			}
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: res})
		case "resources/list":
			items, routes := s.aggregateResources()
			resourceRoutes = routes
			raw, _ := json.Marshal(map[string]any{"resources": items})
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: raw})
		case "resources/templates/list":
			items, routes := s.aggregateResourceTemplates()
			templateRoutes = routes
			raw, _ := json.Marshal(map[string]any{"resourceTemplates": items})
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: raw})
		case "resources/read":
			params := map[string]any{}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: "invalid resources/read params"}})
				continue
			}
			uri, _ := params["uri"].(string)
			route, ok := resourceRoutes[uri]
			if !ok {
				route, ok = templateRoutes[uri]
			}
			if !ok {
				route, ok = parseProxyResourceURI(uri)
			}
			if !ok {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: "resource not found"}})
				continue
			}
			params["uri"] = route.OriginalURI
			res, err := s.forwardResourceRead(route.ServerName, params)
			if err != nil {
				_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32000, Message: err.Error()}})
				continue
			}
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: res})
		default:
			_ = write(rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}})
		}
	}
	if err := in.Err(); err != nil {
		return err
	}
	return nil
}
