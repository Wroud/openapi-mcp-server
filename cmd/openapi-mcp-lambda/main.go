package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
	"github.com/wroud/openapi-mcp-server/proxy"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	config := proxy.Config{SchemaURL: os.Getenv("OPENAPI_URL")}
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
	lambda.Start(httpadapter.NewV2(mux).ProxyWithContext)
}
