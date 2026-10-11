package proxy

import (
	"context"
	"errors"
	"fmt"
	"github.com/getkin/kin-openapi/openapi3"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

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
}

type credentialsKey struct{}

func WithCredentials(ctx context.Context, credentials http.Header) context.Context {
	return context.WithValue(ctx, credentialsKey{}, credentials.Clone())
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
	server := mcp.NewServer(&mcp.Implementation{Name: "openapi-mcp-server", Version: "0.3.0"}, nil)
	if config.BackendURL != "" {
		backend, err := url.Parse(config.BackendURL)
		if err != nil || !validBackend(backend) {
			return nil, errors.New("invalid BackendURL")
		}
	}
	fetch := func(ctx context.Context, req mcp.Request, anonymous bool) (*openapi3.T, http.Header, error) {
		credentials := make(http.Header)
		if !anonymous {
			incoming, _ := ctx.Value(credentialsKey{}).(http.Header)
			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				incoming = extra.Header
			}
			for _, name := range headers {
				for _, value := range incoming.Values(name) {
					credentials.Add(name, value)
				}
			}
		}
		document, err := schemaLoader.Load(ctx, credentials)
		if err != nil {
			slog.ErrorContext(ctx, "API schema unavailable", "metric_name", "UnhandledHttpError", "error", err)
			return nil, nil, errors.New("API information is temporarily unavailable")
		}
		return document, credentials, nil
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_api_information",
		Description: "Get the current API title, version and description visible to the caller.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, APIInfo, error) {
		document, _, err := fetch(ctx, req, false)
		if err != nil {
			return nil, APIInfo{}, err
		}
		return nil, APIInfo{Title: document.Info.Title, Version: document.Info.Version, Description: document.Info.Description}, nil
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
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
							return Execute(ctx, schemaLoader.client, operation, call.Arguments, credentials), nil
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
					listing.Tools = append(listing.Tools, operation.Tool)
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

func NewHTTPHandler(server *mcp.Server) http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBytes)
		w.Header().Set("Cache-Control", "no-store")
		transport.ServeHTTP(w, r)
	})
}
