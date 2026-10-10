package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080/mcp", "Local MCP endpoint")
	flag.Parse()
	if err := run(*endpoint); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "get_api_information" {
		return fmt.Errorf("expected get_api_information as the only tool, got %d tools", len(tools.Tools))
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_api_information", Arguments: map[string]any{}})
	if err != nil {
		return fmt.Errorf("call tool: %w", err)
	}
	if result.IsError {
		return fmt.Errorf("metadata tool failed: %v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	var info struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(encoded, &info); err != nil {
		return err
	}
	if info.Title == "" || info.Version == "" {
		return fmt.Errorf("tool returned incomplete metadata")
	}
	output := struct {
		Endpoint string `json:"endpoint"`
		Tool     string `json:"tool"`
		API      any    `json:"api"`
	}{Endpoint: endpoint, Tool: tools.Tools[0].Name, API: result.StructuredContent}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
