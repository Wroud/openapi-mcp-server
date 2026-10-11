package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wroud/openapi-mcp-server/proxy"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	transport := flag.String("transport", "http", "MCP transport: http or stdio")
	flag.Parse()
	config := proxy.Config{SchemaURL: os.Getenv("OPENAPI_URL"), BackendURL: os.Getenv("BACKEND_URL")}
	if headers, ok := os.LookupEnv("AUTH_HEADERS"); ok {
		config.AuthHeaders = []string{}
		if headers != "" {
			config.AuthHeaders = strings.Split(headers, ",")
		}
	}
	server, err := proxy.NewServer(config)
	if err != nil {
		slog.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}
	if *transport == "stdio" {
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			slog.Error("MCP server stopped", "error", err)
			os.Exit(1)
		}
		return
	}
	if *transport != "http" {
		slog.Error("Unknown MCP transport", "transport", *transport)
		os.Exit(1)
	}
	endpoint := os.Getenv("MCP_PATH")
	if endpoint == "" {
		endpoint = "/mcp"
	}
	if !strings.HasPrefix(endpoint, "/") || strings.ContainsAny(endpoint, "{}?# ") {
		slog.Error("MCP_PATH must be an absolute URL path")
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.Handle(endpoint, proxy.NewHTTPHandler(server))
	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	httpServer := &http.Server{
		Addr: address, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		slog.Error("HTTP server stopped", "error", err)
		os.Exit(1)
	}
}
