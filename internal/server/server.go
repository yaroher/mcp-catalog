package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naukograd-software/mcp-catalog/internal/config"
	"github.com/naukograd-software/mcp-catalog/internal/manager"
)

//go:embed all:static
var staticFiles embed.FS

const proxyServerName = "mcp-catalog-proxy"

type Server struct {
	store    *config.Store
	mgr      *manager.Manager
	clients  map[*websocket.Conn]*wsClient
	mu       sync.RWMutex
	proxyMu  sync.RWMutex
	proxy    manager.ServerInfo
	mcpMu    sync.RWMutex
	mcpState map[string]*mcpSession
	upgrader websocket.Upgrader
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	mu   sync.Mutex
	once sync.Once
}

func (c *wsClient) writeLoop() {
	for msg := range c.send {
		c.mu.Lock()
		err := c.conn.WriteMessage(websocket.TextMessage, msg)
		c.mu.Unlock()
		if err != nil {
			break
		}
	}
	_ = c.close()
}

func (c *wsClient) writeText(msg []byte) error {
	select {
	case c.send <- msg:
		return nil
	default:
		return fmt.Errorf("client buffer full")
	}
}

func (c *wsClient) close() error {
	var err error
	c.once.Do(func() {
		close(c.send)
		c.mu.Lock()
		err = c.conn.Close()
		c.mu.Unlock()
	})
	return err
}

func New(store *config.Store, mgr *manager.Manager) *Server {
	s := &Server{
		store:    store,
		mgr:      mgr,
		clients:  make(map[*websocket.Conn]*wsClient),
		mcpState: make(map[string]*mcpSession),
		proxy: manager.ServerInfo{
			Name:   proxyServerName,
			Status: manager.StatusHealthy,
			Logs:   []manager.LogEntry{},
		},
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	// Subscribe to manager events
	mgr.OnChange(func(name string, info *manager.ServerInfo) {
		s.broadcast(map[string]interface{}{
			"type":   "server_update",
			"name":   name,
			"server": info,
		})
	})

	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/servers", s.handleServers)
	mux.HandleFunc("/api/servers/", s.handleServer)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/config/export", s.handleExport)
	mux.HandleFunc("/api/config/import", s.handleImport)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/", s.handleToolAction)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/mcp", s.handleMCPProxy)

	// Static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	return recoveryMiddleware(mux)
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic recovered: %v [%s %s]", err, r.Method, r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// GET /api/servers - list all servers with status
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}

	info := s.mgr.GetAllInfo()
	info[proxyServerName] = s.proxyServerInfo(r.Host)
	writeJSON(w, info)
}

// /api/servers/{name} - manage a specific server
func (s *Server) handleServer(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/servers/")
	parts := strings.SplitN(name, "/", 2)
	name = parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if name == proxyServerName {
		switch r.Method {
		case "GET":
			writeJSON(w, s.proxyServerInfo(r.Host))
		case "POST":
			if action != "check" {
				http.Error(w, "unknown action", 400)
				return
			}
			go s.checkProxy(r.Host)
			writeJSON(w, map[string]string{"status": "ok"})
		default:
			http.Error(w, "system proxy entry is read-only", http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case "GET":
		info, ok := s.mgr.GetInfo(name)
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, info)

	case "PUT":
		// Add or update server
		var srv config.MCPServer
		if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.AddServer(name, &srv); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if srv.Enabled {
			go s.mgr.Check(name)
		}
		writeJSON(w, map[string]string{"status": "ok"})

	case "DELETE":
		s.mgr.RemoveServer(name)
		if err := s.store.RemoveServer(name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})

	case "POST":
		switch action {
		case "check":
			go s.mgr.Check(name)
			writeJSON(w, map[string]string{"status": "ok"})
		default:
			http.Error(w, "unknown action", 400)
		}

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// GET /api/config - get full config
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := s.store.Get()
		writeJSON(w, cfg)
	case "PUT":
		var cfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.Set(&cfg); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// GET /api/config/export
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.Export()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=mcp-servers.json")
	w.Write(data)
}

// POST /api/config/import
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.Set(&cfg); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// GET /api/tools - list installed CLI tools
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	tools := s.mgr.DetectTools()
	writeJSON(w, tools)
}

// /api/tools/{name}/guide
func (s *Server) handleToolAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/tools/")
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "guide":
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		guide, err := s.mgr.ToolGuide(name)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, guide)

	default:
		http.Error(w, "unknown action", 400)
	}
}

// GET/PUT /api/settings
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, map[string]int{
			"healthCheckInterval": s.store.GetHealthCheckInterval(),
		})
	case "PUT":
		var body struct {
			HealthCheckInterval int `json:"healthCheckInterval"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.SetHealthCheckInterval(body.HealthCheckInterval); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.mgr.SetHealthInterval(body.HealthCheckInterval)
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// WebSocket handler
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}

	client := &wsClient{
		conn: conn,
		send: make(chan []byte, 256),
	}
	go client.writeLoop()

	s.addClient(client)
	defer func() {
		if c := s.removeClient(conn); c != nil {
			_ = c.close()
		}
	}()

	// Send initial state
	info := s.mgr.GetAllInfo()
	info[proxyServerName] = s.proxyServerInfo(r.Host)
	msg, _ := json.Marshal(map[string]interface{}{
		"type":    "initial",
		"servers": info,
	})
	if err := client.writeText(msg); err != nil {
		return
	}

	// Read loop (keep alive)
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func (s *Server) proxyServerInfo(host string) *manager.ServerInfo {
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1:9847"
	}
	tools, _ := s.aggregateTools()
	prompts, _ := s.aggregatePrompts()
	resources, _ := s.aggregateResources()

	outTools := make([]manager.MCPTool, 0, len(tools))
	for _, t := range tools {
		outTools = append(outTools, manager.MCPTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	outPrompts := make([]manager.MCPPrompt, 0, len(prompts))
	for _, p := range prompts {
		name, _ := p["name"].(string)
		desc, _ := p["description"].(string)
		if name == "" {
			continue
		}
		outPrompts = append(outPrompts, manager.MCPPrompt{
			Name:        name,
			Description: desc,
		})
	}

	outResources := make([]manager.MCPResource, 0, len(resources))
	for _, r := range resources {
		uri, _ := r["uri"].(string)
		name, _ := r["name"].(string)
		desc, _ := r["description"].(string)
		mimeType, _ := r["mimeType"].(string)
		if uri == "" {
			continue
		}
		outResources = append(outResources, manager.MCPResource{
			Name:        name,
			URI:         uri,
			Description: desc,
			MimeType:    mimeType,
		})
	}

	s.proxyMu.RLock()
	state := s.proxy
	s.proxyMu.RUnlock()

	return &manager.ServerInfo{
		Name:    proxyServerName,
		Virtual: true,
		Config: config.MCPServer{
			Type:    "streamableHttp",
			URL:     fmt.Sprintf("http://%s/mcp", host),
			Command: "/usr/local/bin/mcp-manager",
			Args:    []string{"--mcp-stdio"},
			Enabled: true,
		},
		Status:          state.Status,
		Error:           state.Error,
		Logs:            append([]manager.LogEntry{}, state.Logs...),
		Tools:           outTools,
		Prompts:         outPrompts,
		Resources:       outResources,
		LastCheck:       state.LastCheck,
		CheckDuration:   state.CheckDuration,
		ServerName:      "mcp-catalog-proxy",
		ServerVersion:   "1.0.0",
		ProtocolVersion: "2024-11-05",
	}
}

func (s *Server) checkProxy(host string) {
	start := time.Now()
	info := s.proxyServerInfo(host)
	info.Status = manager.StatusChecking
	info.Error = ""
	info.Logs = append(info.Logs, manager.LogEntry{
		Time:    time.Now(),
		Level:   "info",
		Message: "Checking aggregated proxy backends",
	})
	s.setProxyState(info)

	cfg := s.store.Get()
	enabled := 0
	okCount := 0
	var failures []string
	for name, srv := range cfg.MCPServers {
		if srv == nil || !srv.Enabled {
			continue
		}
		enabled++
		if _, err := s.forwardMCP(name, srv, "tools/list", map[string]any{}); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		okCount++
	}

	now := time.Now()
	info = s.proxyServerInfo(host)
	info.LastCheck = &now
	info.CheckDuration = time.Since(start).Milliseconds()
	if enabled > 0 && okCount == 0 {
		info.Status = manager.StatusError
		if len(failures) > 0 {
			info.Error = strings.Join(failures, " | ")
		} else {
			info.Error = "no reachable upstream MCP servers"
		}
		info.Logs = append(info.Logs, manager.LogEntry{
			Time:    time.Now(),
			Level:   "error",
			Message: info.Error,
		})
	} else {
		info.Status = manager.StatusHealthy
		info.Error = ""
		msg := fmt.Sprintf("Proxy check OK: %d/%d upstream servers reachable", okCount, enabled)
		info.Logs = append(info.Logs, manager.LogEntry{
			Time:    time.Now(),
			Level:   "info",
			Message: msg,
		})
	}
	s.setProxyState(info)
}

func (s *Server) setProxyState(info *manager.ServerInfo) {
	if info == nil {
		return
	}
	s.proxyMu.Lock()
	s.proxy.Status = info.Status
	s.proxy.Error = info.Error
	s.proxy.LastCheck = info.LastCheck
	s.proxy.CheckDuration = info.CheckDuration
	s.proxy.Logs = append([]manager.LogEntry{}, info.Logs...)
	s.proxyMu.Unlock()

	s.broadcast(map[string]interface{}{
		"type":   "server_update",
		"name":   proxyServerName,
		"server": info,
	})
}

func (s *Server) broadcast(data interface{}) {
	msg, err := json.Marshal(data)
	if err != nil {
		return
	}

	for _, client := range s.snapshotClients() {
		if err := client.writeText(msg); err != nil {
			if c := s.removeClient(client.conn); c != nil {
				_ = c.close()
			}
		}
	}
}

func (s *Server) addClient(client *wsClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[client.conn] = client
}

func (s *Server) removeClient(conn *websocket.Conn) *wsClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.clients[conn]
	delete(s.clients, conn)
	return client
}

func (s *Server) snapshotClients() []*wsClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*wsClient, 0, len(s.clients))
	for _, client := range s.clients {
		out = append(out, client)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
