package proxy

import (
	"context"
	"errors"
	"fmt"
	"github.com/getkin/kin-openapi/openapi3"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const MaxBytes = 1 << 20

type APIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type Config struct {
	SchemaURL     string
	BackendURL    string
	AuthHeaders   []string
	SchemaOptions SchemaOptions

	AnonymousSchema               bool
	ResourceMetadataURL           string
	ChallengeScope                string
	ToolResultChallengeUserAgents []string
	SchemaCacheTTL                time.Duration
}

type credentialsKey struct{}

func WithCredentials(ctx context.Context, credentials http.Header) context.Context {
	return context.WithValue(ctx, credentialsKey{}, credentials.Clone())
}

func requestHeaders(ctx context.Context, req mcp.Request) http.Header {
	incoming, _ := ctx.Value(credentialsKey{}).(http.Header)
	if extra := req.GetExtra(); extra != nil && extra.Header != nil {
		incoming = extra.Header
	}
	if incoming == nil {
		incoming = make(http.Header)
	}
	return incoming
}

func NewServer(config Config) (*mcp.Server, error) {
	schemaLoader, err := NewSchemaLoader(config.SchemaURL, config.SchemaOptions)
	if err != nil {
		return nil, err
	}
	headers := config.AuthHeaders
	if headers == nil {
		headers = []string{"Authorization", "Cookie", "X-Api-Key"}
	}
	headers = append([]string(nil), headers...)
	for i, name := range headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !validAuthHeader(name) {
			return nil, fmt.Errorf("invalid authentication header %q", name)
		}
		headers[i] = name
	}
	challenges, err := newChallenger(config)
	if err != nil {
		return nil, err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "openapi-mcp-server", Version: "0.4.1"}, nil)
	if config.BackendURL != "" {
		backend, err := url.Parse(config.BackendURL)
		if err != nil || !validBackend(backend) {
			return nil, errors.New("invalid BackendURL")
		}
	}
	if config.SchemaCacheTTL < 0 {
		return nil, errors.New("SchemaCacheTTL must not be negative")
	}
	var cache *schemaCache
	if config.AnonymousSchema && config.SchemaCacheTTL > 0 {
		cache = &schemaCache{loader: schemaLoader, ttl: config.SchemaCacheTTL}
	}
	fetch := func(ctx context.Context, req mcp.Request, anonymous bool) (*openapi3.T, http.Header, error) {
		credentials := make(http.Header)
		if !anonymous {
			incoming := requestHeaders(ctx, req)
			for _, name := range headers {
				for _, value := range incoming.Values(name) {
					credentials.Add(name, value)
				}
			}
		}
		schemaCredentials := credentials
		if config.AnonymousSchema {
			schemaCredentials = nil
		}
		var document *openapi3.T
		var err error
		if cache != nil {
			document, err = cache.load(ctx)
		} else {
			document, err = schemaLoader.Load(ctx, schemaCredentials)
		}
		if err != nil {
			slog.ErrorContext(ctx, "API schema unavailable", "metric_name", "UnhandledHttpError", "error", err)
			return nil, nil, errors.New("API information is temporarily unavailable")
		}
		return document, credentials, nil
	}
	information := &mcp.Tool{
		Name:        "get_api_information",
		Description: "Get the current API title, version and description visible to the caller.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}
	if challenges != nil {
		information.Meta = challenges.schemes(false)
	}
	mcp.AddTool(server, information, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, APIInfo, error) {
		document, _, err := fetch(ctx, req, false)
		if err != nil {
			return nil, APIInfo{}, err
		}
		return nil, APIInfo{Title: document.Info.Title, Version: document.Info.Version, Description: document.Info.Description}, nil
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				logClient(ctx, req)
			}
			if method == "initialize" || method == "server/discover" {
				if _, _, err := fetch(ctx, req, true); err != nil {
					return nil, err
				}
			}
			var call *mcp.CallToolParamsRaw
			if method == "tools/call" {
				var ok bool
				call, ok = req.GetParams().(*mcp.CallToolParamsRaw)
				if !ok {
					return nil, errors.New("invalid tool call parameters")
				}
				if call.Name == "get_api_information" {
					return next(ctx, method, req)
				}
			}
			if method == "tools/list" || call != nil {
				if method == "tools/list" {
					if params, ok := req.GetParams().(*mcp.ListToolsParams); ok && params != nil && params.Cursor != "" {
						return nil, errors.New("tool pagination is not supported")
					}
				}
				document, credentials, err := fetch(ctx, req, false)
				if err != nil {
					return nil, err
				}
				catalog, err := BuildCatalog(document, config.SchemaURL, config.BackendURL)
				if err != nil {
					slog.ErrorContext(ctx, "Operation catalog unavailable", "error", err)
					return nil, errors.New("API tools are temporarily unavailable")
				}
				if call != nil {
					for _, operation := range catalog.Operations {
						if operation.Tool.Name == call.Name {
							incoming := requestHeaders(ctx, req)
							if challenges != nil && operation.Protected && len(credentials) == 0 {
								return challenges.respond(ctx, call.Name, incoming, false), nil
							}
							result, status := execute(ctx, schemaLoader.client, operation, call.Arguments, credentials)
							if challenges != nil && operation.Protected && len(credentials) > 0 && status == http.StatusUnauthorized {
								return challenges.respond(ctx, call.Name, incoming, true), nil
							}
							return result, nil
						}
					}
					return next(ctx, method, req)
				}
				result, err := next(ctx, method, req)
				if err != nil {
					return nil, err
				}
				listing := result.(*mcp.ListToolsResult)
				names := make(map[string]bool)
				for _, tool := range listing.Tools {
					names[tool.Name] = true
				}
				for _, operation := range catalog.Operations {
					if names[operation.Tool.Name] {
						return nil, errors.New("operation collides with an existing MCP tool")
					}
					tool := operation.Tool
					if challenges != nil {
						listed := *tool
						listed.Meta = challenges.schemes(operation.Protected)
						tool = &listed
					}
					listing.Tools = append(listing.Tools, tool)
				}
				listing.CacheScope = "private"
				listing.TTLMs = 0
				return listing, nil
			}
			return next(ctx, method, req)
		}
	})

	return server, nil
}

func logClient(ctx context.Context, req mcp.Request) {
	name, version := "", ""
	if params, ok := req.GetParams().(*mcp.InitializeParams); ok && params != nil && params.ClientInfo != nil {
		name, version = params.ClientInfo.Name, params.ClientInfo.Version
	}
	slog.InfoContext(ctx, "MCP client initialized", "user_agent", requestHeaders(ctx, req).Get("User-Agent"), "client_name", name, "client_version", version)
}

func ConfigFromEnv() (Config, error) {
	config := Config{
		SchemaURL:           os.Getenv("OPENAPI_URL"),
		BackendURL:          os.Getenv("BACKEND_URL"),
		AnonymousSchema:     os.Getenv("ANONYMOUS_SCHEMA") == "true",
		ResourceMetadataURL: os.Getenv("RESOURCE_METADATA_URL"),
		ChallengeScope:      os.Getenv("CHALLENGE_SCOPE"),
	}
	for name, target := range map[string]*time.Duration{"SCHEMA_CACHE_TTL": &config.SchemaCacheTTL, "SCHEMA_TIMEOUT": &config.SchemaOptions.Limits.Timeout, "SCHEMA_SOURCE_TIMEOUT": &config.SchemaOptions.Limits.SourceTimeout} {
		if value := os.Getenv(name); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return config, fmt.Errorf("invalid %s: %w", name, err)
			}
			*target = parsed
		}
	}
	if agents := os.Getenv("TOOL_RESULT_CHALLENGE_USER_AGENTS"); agents != "" {
		config.ToolResultChallengeUserAgents = strings.Split(agents, ",")
	}
	if headers, ok := os.LookupEnv("AUTH_HEADERS"); ok {
		config.AuthHeaders = []string{}
		if headers != "" {
			config.AuthHeaders = strings.Split(headers, ",")
		}
	}
	return config, nil
}

func validAuthHeader(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char)) {
			return false
		}
	}
	switch name {
	case "Host", "Connection", "Content-Length", "Content-Type", "Accept", "Transfer-Encoding", "Upgrade", "Trailer", "Te", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "Accept-Encoding":
		return false
	}
	return true
}

func NewHandler(schemaURL string) (http.Handler, error) {
	server, err := NewServer(Config{SchemaURL: schemaURL})
	if err != nil {
		return nil, err
	}
	return NewHTTPHandler(server), nil
}
