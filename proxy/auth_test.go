package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func rpc(t *testing.T, handler http.Handler, method string, headers http.Header) map[string]json.RawMessage {
	t.Helper()
	params := `{}`
	if method == "initialize" {
		params = `{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`
	}
	if method == "tools/call" {
		params = `{"name":"get_api_information","arguments":{}}`
	}
	request := httptest.NewRequest("POST", "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)))
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("%s HTTP %d: %s", method, response.Code, response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestConcurrentCredentialIsolationAndFreshness(t *testing.T) {
	var active, peak, anonymous, authenticated atomic.Int32
	var version atomic.Int32
	version.Store(1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old; old = peak.Load() {
			if peak.CompareAndSwap(old, count) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if r.Header.Get("X-Unrelated") != "" {
			t.Error("forwarded unconfigured header")
		}
		identity := strings.Join([]string{r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Tenant-Token")}, "|")
		if identity == "|||" {
			anonymous.Add(1)
		} else {
			authenticated.Add(1)
		}
		json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "info": APIInfo{Title: identity, Version: fmt.Sprint(version.Load()), Description: "Current caller schema"}})
	}))
	defer upstream.Close()
	server, err := NewServer(Config{SchemaURL: upstream.URL, AuthHeaders: []string{"Authorization", "Cookie", "X-Api-Key", "X-Tenant-Token"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(server)
	initHeaders := http.Header{"Authorization": {"Bearer init"}, "Cookie": {"session=init"}, "X-Api-Key": {"init"}}
	result := rpc(t, handler, "initialize", initHeaders)
	if result["error"] != nil || anonymous.Load() != 1 || authenticated.Load() != 0 {
		t.Fatalf("initialization was not anonymous: %s", result)
	}
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			headers := make(http.Header)
			name := []string{"Authorization", "Cookie", "X-Api-Key", "X-Tenant-Token"}[i%4]
			headers.Set(name, fmt.Sprintf("user-%d", i))
			headers.Set("X-Unrelated", "do not forward")
			listing := rpc(t, handler, "tools/list", headers)
			if listing["error"] != nil {
				t.Errorf("listing failed: %s", listing)
				return
			}
			response := rpc(t, handler, "tools/call", headers)
			var output struct {
				StructuredContent APIInfo `json:"structuredContent"`
			}
			if err := json.Unmarshal(response["result"], &output); err != nil {
				t.Error(err)
				return
			}
			expected := strings.Join([]string{headers.Get("Authorization"), headers.Get("Cookie"), headers.Get("X-Api-Key"), headers.Get("X-Tenant-Token")}, "|")
			if output.StructuredContent.Title != expected {
				t.Errorf("credential leak: got %q want %q", output.StructuredContent.Title, expected)
			}
		}()
	}
	group.Wait()
	if peak.Load() < 2 || authenticated.Load() != 64 {
		t.Fatalf("peak=%d authenticated=%d", peak.Load(), authenticated.Load())
	}
	version.Store(2)
	response := rpc(t, handler, "tools/call", nil)
	var output struct {
		StructuredContent APIInfo `json:"structuredContent"`
	}
	if err := json.Unmarshal(response["result"], &output); err != nil {
		t.Fatal(err)
	}
	if output.StructuredContent.Version != "2" || output.StructuredContent.Title != "|||" {
		t.Fatalf("stale or authenticated anonymous result: %+v", output)
	}
}

func TestTransportIndependentServer(t *testing.T) {
	var anonymous atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := r.Header.Get("Authorization")
		if identity == "" {
			identity = "anonymous"
			anonymous.Add(1)
		}
		json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "info": APIInfo{Title: identity, Version: "1"}})
	}))
	defer upstream.Close()
	server, err := NewServer(Config{SchemaURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, identity := range []string{"Bearer alice", "Bearer bob"} {
		headers := http.Header{"Authorization": {identity}}
		callerContext := WithCredentials(ctx, headers)
		headers.Set("Authorization", "mutated")
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		serverSession, err := server.Connect(callerContext, serverTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer serverSession.Close()
		client := mcp.NewClient(&mcp.Implementation{Name: "embedded", Version: "1"}, nil)
		session, err := client.Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_api_information", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			t.Fatalf("call: %+v %v", result, err)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var info APIInfo
		if err := json.Unmarshal(encoded, &info); err != nil {
			t.Fatal(err)
		}
		if info.Title != identity {
			t.Fatalf("context credentials: got %q want %q", info.Title, identity)
		}
	}
	if anonymous.Load() != 2 {
		t.Fatalf("anonymous initialization calls: %d", anonymous.Load())
	}
}

func TestAuthenticationFailureDoesNotUseAnonymousSchema(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "info": APIInfo{Title: "Anonymous API", Version: "1"}})
	}))
	defer upstream.Close()
	handler, err := NewHandler(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rejected := http.Header{"Authorization": {"Bearer rejected"}}
	if result := rpc(t, handler, "initialize", rejected); result["error"] != nil {
		t.Fatalf("anonymous init: %s", result)
	}
	if result := rpc(t, handler, "tools/list", rejected); result["error"] == nil {
		t.Fatal("authenticated listing unexpectedly succeeded")
	}
	result := rpc(t, handler, "tools/call", rejected)
	var output struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result["result"], &output); err != nil {
		t.Fatal(err)
	}
	if !output.IsError {
		t.Fatal("authenticated tool used anonymous schema")
	}
	if result := rpc(t, handler, "tools/list", nil); result["error"] != nil {
		t.Fatalf("anonymous recovery: %s", result)
	}
}
