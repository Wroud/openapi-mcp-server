package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const MaxBytes = 1 << 20

type APIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type Config struct {
	SchemaURL   string
	AuthHeaders []string
}

type credentialsKey struct{}

func WithCredentials(ctx context.Context, credentials http.Header) context.Context {
	return context.WithValue(ctx, credentialsKey{}, credentials.Clone())
}

func NewServer(config Config) (*mcp.Server, error) {
	u, err := url.Parse(config.SchemaURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return nil, errors.New("OPENAPI_URL must be an absolute HTTP(S) URL without credentials or a fragment")
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
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "openapi-mcp-server", Version: "0.1.0"}, nil)
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
		info, err := loadInfo(ctx, client, config.SchemaURL, credentials)
		if err != nil {
			slog.ErrorContext(ctx, "API schema unavailable", "metric_name", "UnhandledHttpError", "error", err)
			return APIInfo{}, errors.New("API information is temporarily unavailable")
		}
		return info, nil
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

func loadInfo(ctx context.Context, client *http.Client, schemaURL string, credentials http.Header) (APIInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, schemaURL, nil)
	if err != nil {
		return APIInfo{}, err
	}
	req.Header = credentials.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return APIInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return APIInfo{}, fmt.Errorf("schema returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return APIInfo{}, err
	}
	if len(data) > MaxBytes {
		return APIInfo{}, errors.New("schema exceeds size limit")
	}
	var root struct {
		OpenAPI string         `json:"openapi"`
		Info    *openapi3.Info `json:"info"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return APIInfo{}, err
	}
	if root.OpenAPI != "3.0.0" && root.OpenAPI != "3.0.1" && root.OpenAPI != "3.0.2" && root.OpenAPI != "3.0.3" && root.OpenAPI != "3.0.4" && root.OpenAPI != "3.1.0" && root.OpenAPI != "3.1.1" && root.OpenAPI != "3.1.2" {
		return APIInfo{}, errors.New("unsupported OpenAPI version")
	}
	if root.Info == nil {
		return APIInfo{}, errors.New("schema has no info")
	}
	if err := root.Info.Validate(ctx); err != nil {
		return APIInfo{}, err
	}
	return APIInfo{Title: root.Info.Title, Version: root.Info.Version, Description: root.Info.Description}, nil
}
