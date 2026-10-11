package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSDKMetadata(t *testing.T) {
	root, err := os.ReadFile("testdata/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Info APIInfo `json:"info"`
	}
	if err := json.Unmarshal(root, &expected); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	var unavailable atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if r.URL.Path != "/openapi.json" {
			t.Errorf("unexpected reference fetch: %s", r.URL.Path)
		}
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(root)
	}))
	defer upstream.Close()
	handler, err := NewHandler(upstream.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: host.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if fetches.Load() == 0 {
		t.Fatal("SDK initialization did not fetch the anonymous schema")
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 1 || tools.Tools[0].Name != "get_api_information" {
		t.Fatalf("tools: %+v", tools.Tools)
	}
	beforeCall := fetches.Load()
	if beforeCall < 1 {
		t.Fatal("initialization/discovery did not fetch schema")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_api_information", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("call: %+v %v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var actual APIInfo
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if actual != expected.Info || fetches.Load() != beforeCall+1 {
		t.Fatalf("info: %+v; fetches: %d", actual, fetches.Load())
	}
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "unknown", Arguments: map[string]any{}})
	if err == nil || fetches.Load() != beforeCall+2 {
		t.Fatal("unknown tool did not refresh the current schema")
	}
	unavailable.Store(true)
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_api_information", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatalf("outage: %+v %v", result, err)
	}
	unavailable.Store(false)
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_api_information", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("recovery: %+v %v", result, err)
	}
}

func TestLoadInfoFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"status", "", 503},
		{"invalid", "{", 200},
		{"missing", `{"openapi":"3.1.0"}`, 200},
		{"title", `{"openapi":"3.1.0","info":{"version":"1"}}`, 200},
		{"version", `{"openapi":"3.1.0","info":{"title":"API"}}`, 200},
		{"unsupported", `{"openapi":"2.0","info":{"title":"API","version":"1"}}`, 200},
		{"oversize", strings.Repeat(" ", MaxBytes+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer host.Close()
			if _, err := loadInfo(context.Background(), host.Client(), host.URL, nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCancellationAndRedirect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadInfo(ctx, http.DefaultClient, "http://127.0.0.1:1", nil); err == nil {
		t.Fatal("expected cancellation")
	}
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer origin.Close()
	handler, err := NewHandler(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_api_information","arguments":{}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "temporarily unavailable") {
		t.Fatalf("redirect response: %d %s", response.Code, response.Body.String())
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("followed redirect")
	}
}

func TestLambdaEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"}}`)
	}))
	defer upstream.Close()
	handler, err := NewHandler(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	adapter := httpadapter.NewV2(handler)
	for _, tc := range []struct {
		method, body string
		status       int
		base64       bool
	}{
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, 200, true},
		{"POST", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200, false},
		{"POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202, false},
		{"GET", "", 405, false},
		{"DELETE", "", 405, false},
		{"POST", strings.Repeat(" ", MaxBytes+1), 413, false},
	} {
		t.Run(tc.method+tc.body[:min(len(tc.body), 24)], func(t *testing.T) {
			body := tc.body
			if tc.base64 {
				body = base64.StdEncoding.EncodeToString([]byte(body))
			}
			event := events.APIGatewayV2HTTPRequest{Version: "2.0", RawPath: "/mcp", Body: body, IsBase64Encoded: tc.base64,
				Headers:        map[string]string{"content-type": "application/json", "accept": "application/json, text/event-stream", "mcp-protocol-version": "2025-03-26"},
				RequestContext: events.APIGatewayV2HTTPRequestContext{HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: tc.method, Path: "/mcp"}},
			}
			response, err := adapter.ProxyWithContext(context.Background(), event)
			if err != nil || response.StatusCode != tc.status {
				t.Fatalf("response: %+v %v", response, err)
			}
			if tc.status == 200 && !json.Valid([]byte(response.Body)) {
				t.Fatalf("not buffered JSON: %s", response.Body)
			}
		})
	}
}
