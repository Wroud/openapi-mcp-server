package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	server := mcp.NewServer(&mcp.Implementation{Name: "openapi-mcp-server", Version: "0.2.0"}, nil)
	fetch := func(ctx context.Context, req mcp.Request, anonymous bool) (APIInfo, error) {
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
			return APIInfo{}, errors.New("API information is temporarily unavailable")
		}
		return APIInfo{Title: document.Info.Title, Version: document.Info.Version, Description: document.Info.Description}, nil
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_api_information",
		Description: "Get the current API title, version and description visible to the caller.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, APIInfo, error) {
		info, err := fetch(ctx, req, false)
		return nil, info, err
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" || method == "server/discover" || method == "tools/list" {
				if _, err := fetch(ctx, req, method != "tools/list"); err != nil {
					return nil, err
				}
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
