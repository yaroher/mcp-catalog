package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/yaroher/mcp-catalog/internal/config"
	"github.com/yaroher/mcp-catalog/internal/manager"
	"github.com/yaroher/mcp-catalog/internal/server"
)

func main() {
	log.SetOutput(os.Stderr)
	port := flag.Int("port", 9847, "HTTP port")
	configPath := flag.String("config", "", "Config file path (default: ~/.config/mcp-manager/config.json)")
	mcpStdio := flag.Bool("mcp-stdio", false, "Run as MCP proxy over stdio")
	flag.Parse()

	if *configPath == "" {
		home, _ := os.UserHomeDir()
		*configPath = filepath.Join(home, ".config", "mcp-manager", "config.json")
	}

	// Ensure config directory exists
	os.MkdirAll(filepath.Dir(*configPath), 0755)

	// Initialize config store
	store := config.NewStore(*configPath)
	if err := store.Load(); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Config loaded from %s", *configPath)

	// Initialize manager
	mgr := manager.New(store)

	if *mcpStdio {
		log.Printf("Starting MCP proxy over stdio")
		if err := server.RunMCPStdio(store); err != nil {
			log.Fatalf("Stdio MCP server error: %v", err)
		}
		return
	}

	// Initial health check for all enabled servers
	go mgr.CheckAll()

	// Start periodic health check loop
	go mgr.StartHealthLoop()

	// Initialize HTTP server
	srv := server.New(store, mgr)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("MCP Manager UI: http://localhost%s", addr)

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		mgr.StopHealthLoop()
		os.Exit(0)
	}()

	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
