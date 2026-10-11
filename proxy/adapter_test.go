package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const adapterSchema = `{"openapi":"3.1.0","info":{"title":"Example","version":"1"},"servers":[{"url":"/v1"}],"paths":{"/items/{id}":{"parameters":[{"in":"path","name":"id","required":true,"schema":{"type":"string","minLength":1}}],"get":{"operationId":"readItem","description":"Read one item","parameters":[{"in":"query","name":"tag","schema":{"type":"array","items":{"type":"string"}}},{"in":"query","name":"active","schema":{"type":"boolean"}}],"responses":{"200":{"description":"Item"}}},"delete":{"operationId":"deleteItem","responses":{"204":{"description":"Deleted"}}}},"/items":{"post":{"operationId":"createItem","requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object","additionalProperties":false,"required":["name","count"],"properties":{"name":{"type":"string","minLength":2,"pattern":"^[A-Z]"},"count":{"type":"integer","minimum":1},"note":{"type":["string","null"]}}}}}},"responses":{"200":{"description":"Created"}}}}}}`

func catalogFor(t *testing.T, host *httptest.Server, schema string) Catalog {
	t.Helper()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, schema) }))
	defer source.Close()
	loader, err := NewSchemaLoader(source.URL, SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	document, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	override := ""
	if host != nil {
		override = host.URL + "/v1"
	}
	catalog, err := BuildCatalog(document, source.URL, override)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func operationNamed(t *testing.T, catalog Catalog, name string) Operation {
	t.Helper()
	for _, operation := range catalog.Operations {
		if operation.Tool.Name == name {
			return operation
		}
	}
	t.Fatalf("missing %s; skipped: %+v", name, catalog.Skipped)
	return Operation{}
}

func TestCatalogAndRESTExecution(t *testing.T) {
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer caller" {
			t.Error("caller credentials missing")
		}
		switch r.Method {
		case "GET":
			if r.URL.EscapedPath() != "/v1/items/a%2Fb%20%3F%23" {
				t.Errorf("escaped path: %s", r.URL.EscapedPath())
			}
			if strings.Join(r.URL.Query()["tag"], ",") != "one,two" || r.URL.Query().Get("active") != "false" {
				t.Errorf("query: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"name":"Item"}`)
		case "POST":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["note"] != nil || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("body: %+v", body)
			}
			fmt.Fprint(w, `{"created":true}`)
		case "DELETE":
			w.WriteHeader(204)
		}
	}))
	defer backend.Close()
	catalog := catalogFor(t, backend, adapterSchema)
	if len(catalog.Operations) != 3 || len(catalog.Skipped) != 0 {
		t.Fatalf("catalog: %+v", catalog)
	}
	read := operationNamed(t, catalog, "readItem")
	if read.Tool.Description != "Read one item" || !read.Tool.Annotations.ReadOnlyHint {
		t.Fatal("tool metadata missing")
	}
	input := read.Tool.InputSchema.(map[string]any)
	if !strings.Contains(fmt.Sprint(input["required"]), "path") {
		t.Fatal("required path group missing")
	}
	credentials := http.Header{"Authorization": {"Bearer caller"}}
	for _, tc := range []struct{ name, args string }{
		{"readItem", `{"path":{"id":"a/b ?#"},"query":{"tag":["one","two"],"active":false}}`},
		{"createItem", `{"body":{"name":"Alpha","count":2,"note":null}}`},
		{"deleteItem", `{"path":{"id":"id"}}`},
	} {
		result := Execute(context.Background(), backend.Client(), operationNamed(t, catalog, tc.name), json.RawMessage(tc.args), credentials)
		if result.IsError {
			t.Fatalf("%s: %+v", tc.name, result.Content)
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("requests: %d", requests.Load())
	}
	for _, args := range []string{`{}`, `{"body":{"name":"a","count":0}}`, `{"body":{"name":"Alpha","count":1.5}}`, `{"body":{"name":"Alpha","count":1,"extra":true}}`, `{"headers":{"Authorization":"forged"}}`, `null`} {
		if result := Execute(context.Background(), backend.Client(), operationNamed(t, catalog, "createItem"), json.RawMessage(args), credentials); !result.IsError {
			t.Fatalf("invalid arguments accepted: %s", args)
		}
	}
	for _, args := range []string{`{"path":{"id":".."}}`, `{"path":{"id":"a/../b"}}`, `{"path":{"id":"ok"},"query":{"unknown":true}}`, `{"path":{"id":"ok"},"query":{"tag":[1]}}`} {
		if result := Execute(context.Background(), backend.Client(), read, json.RawMessage(args), credentials); !result.IsError {
			t.Fatalf("invalid read accepted: %s", args)
		}
	}
	if requests.Load() != 3 {
		t.Fatal("invalid inputs reached backend")
	}
}

func TestUnsupportedCatalogOperations(t *testing.T) {
	schema := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/upload":{"post":{"operationId":"uploadFile","requestBody":{"content":{"multipart/form-data":{"schema":{"type":"object"}}}},"responses":{"200":{"description":"ok"}}}},"/search":{"get":{"operationId":"searchProducts","parameters":[{"in":"query","name":"filter","style":"deepObject","schema":{"type":"object"}}],"responses":{"200":{"description":"ok"}}}},"/recursive":{"post":{"operationId":"recursive","requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Node"}}}},"responses":{"200":{"description":"ok"}}}},"/headers":{"get":{"operationId":"headers","parameters":[{"in":"header","name":"X-Option","schema":{"type":"string"}}],"responses":{"200":{"description":"ok"}}}}},"components":{"schemas":{"Node":{"type":"object","properties":{"next":{"$ref":"#/components/schemas/Node"}}}}}}`
	catalog := catalogFor(t, nil, schema)
	if len(catalog.Operations) != 0 || len(catalog.Skipped) != 4 {
		t.Fatalf("catalog: %+v", catalog)
	}
	for _, skipped := range catalog.Skipped {
		if skipped.Reason == "" {
			t.Fatal("missing skip reason")
		}
	}
}

func TestBackendSelectionAndNaming(t *testing.T) {
	schema := strings.ReplaceAll(adapterSchema, `"operationId":"readItem",`, "")
	catalog := catalogFor(t, nil, schema)
	read := operationNamed(t, catalog, "get_items__id_")
	if read.Backend.Path != "/v1" {
		t.Fatalf("base path: %s", read.Backend)
	}
	unsafe := strings.ReplaceAll(adapterSchema, `"url":"/v1"`, `"url":"https://outside.example/"`)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, unsafe) }))
	defer source.Close()
	loader, _ := NewSchemaLoader(source.URL, SchemaOptions{})
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCatalog(doc, source.URL, ""); err == nil {
		t.Fatal("outside-origin server accepted")
	}
	if _, err := BuildCatalog(doc, source.URL, "https://approved.example/base"); err != nil {
		t.Fatal(err)
	}
	collision := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/a-b":{"get":{"responses":{"200":{"description":"ok"}}}},"/a_b":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	another := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, collision) }))
	defer another.Close()
	loader, _ = NewSchemaLoader(another.URL, SchemaOptions{})
	doc, err = loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCatalog(doc, another.URL, ""); err == nil {
		t.Fatal("fallback collision accepted")
	}
}

func TestExecutorErrorsRedirectsLimitsAndCancellation(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		cancel  bool
	}{
		{"error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"token: secret"}`)
		}, false},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }, false},
		{"size", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, strings.Repeat("x", MaxBytes+1)) }, false},
		{"cancel", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(tc.handler)
			defer backend.Close()
			catalog := catalogFor(t, backend, adapterSchema)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if tc.cancel {
				cancel()
			}
			defer cancel()
			result := Execute(ctx, backend.Client(), operationNamed(t, catalog, "readItem"), json.RawMessage(`{"path":{"id":"id"}}`), http.Header{"Authorization": {"Bearer secret"}})
			if !result.IsError {
				t.Fatal("expected error")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "secret") {
				t.Fatal("error echoed credentials")
			}
			if tc.name == "error" && !strings.Contains(string(encoded), "403") {
				t.Fatal("HTTP status was lost")
			}
		})
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("followed backend redirect")
	}
}

func TestCallerScopedCatalogWithSDK(t *testing.T) {
	var backendCalls atomic.Int32
	var revision atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := r.Header.Get("Authorization")
		if r.URL.Path == "/schema" {
			if identity == "" {
				fmt.Fprint(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{}}`)
				return
			}
			name := identity
			if revision.Load() > 0 {
				name += "New"
			}
			fmt.Fprintf(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/%s":{"get":{"operationId":%q,"responses":{"200":{"description":"ok"}}}}}}`, identity, name)
			return
		}
		backendCalls.Add(1)
		if r.URL.Path != "/"+identity {
			t.Errorf("caller mismatch: %s %s", r.URL.Path, identity)
		}
		json.NewEncoder(w).Encode(map[string]string{"caller": identity})
	}))
	defer host.Close()
	server, err := NewServer(Config{SchemaURL: host.URL + "/schema"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(NewHTTPHandler(server))
	defer endpoint.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	anonymous, err := session.ListTools(ctx, nil)
	if err != nil || len(anonymous.Tools) != 1 {
		t.Fatalf("anonymous listing: %+v %v", anonymous, err)
	}
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			identity := fmt.Sprintf("user%d", i)
			headers := http.Header{"Authorization": {identity}}
			listing := rpc(t, NewHTTPHandler(server), "tools/list", headers)
			var tools mcp.ListToolsResult
			if err := json.Unmarshal(listing["result"], &tools); err != nil {
				t.Error(err)
				return
			}
			if len(tools.Tools) != 2 || tools.Tools[1].Name != identity || tools.CacheScope != "private" {
				t.Errorf("caller catalog: %+v", tools)
			}
			for _, name := range []string{identity, "anotherUser"} {
				request := httptest.NewRequest("POST", "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, name)))
				request.Header = headers.Clone()
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("Mcp-Protocol-Version", "2025-03-26")
				response := httptest.NewRecorder()
				NewHTTPHandler(server).ServeHTTP(response, request)
				var envelope map[string]json.RawMessage
				json.Unmarshal(response.Body.Bytes(), &envelope)
				if name == identity {
					var result mcp.CallToolResult
					json.Unmarshal(envelope["result"], &result)
					if result.IsError || result.StructuredContent.(map[string]any)["caller"] != identity {
						t.Error("cross-caller execution")
					}
				} else if envelope["error"] == nil {
					t.Error("executed another caller's tool")
				}
			}
		}()
	}
	group.Wait()
	if backendCalls.Load() != 16 {
		t.Fatalf("backend calls: %d", backendCalls.Load())
	}
	revision.Store(1)
	fresh := rpc(t, NewHTTPHandler(server), "tools/list", http.Header{"Authorization": {"user0"}})
	if !strings.Contains(string(fresh["result"]), "user0New") {
		t.Fatal("stale catalog after schema update")
	}
	request := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"user0","arguments":{}}}`))
	request.Header.Set("Authorization", "user0")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	response := httptest.NewRecorder()
	NewHTTPHandler(server).ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"error"`) || backendCalls.Load() != 16 {
		t.Fatal("stale operation was executed after schema change")
	}
}

func TestReferenceGraphCatalog(t *testing.T) {
	paths := map[string]string{"/openapi.json": "root.json", "/api/pricing/openapi.json": "pricing.json", "/api/loader-support/openapi.json": "loader.json", "/api/bedrock-support/openapi.json": "bedrock.json", "/api/blog/openapi.json": "blog.json", "/api/server-brief/openapi.json": "brief.json"}
	documents := map[string]string{}
	for path, file := range paths {
		data, err := os.ReadFile("testdata/reference-graph/" + file)
		if err != nil {
			t.Fatal(err)
		}
		documents[path] = string(data)
	}
	host, _, _ := schemaTestHost(t, documents)
	loader, _ := NewSchemaLoader(host.URL+"/openapi.json", SchemaOptions{})
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := BuildCatalog(doc, host.URL+"/openapi.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Operations) != 7 || len(catalog.Skipped) > 0 {
		t.Fatalf("catalog: %+v", catalog)
	}
}

func TestEmptyQueryArrayIsNotSilentlyDropped(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer backend.Close()
	catalog := catalogFor(t, backend, adapterSchema)
	result := Execute(context.Background(), backend.Client(), operationNamed(t, catalog, "readItem"), json.RawMessage(`{"path":{"id":"id"},"query":{"tag":[]}}`), nil)
	if !result.IsError || calls.Load() != 0 {
		t.Fatal("empty query array reached the backend as an absent parameter")
	}
}

func TestSDKExecutesGeneratedTools(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/schema" {
			fmt.Fprint(w, adapterSchema)
			return
		}
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer caller" {
			t.Error("SDK caller credentials not forwarded")
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer host.Close()
	server, err := NewServer(Config{SchemaURL: host.URL + "/schema"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(WithCredentials(ctx, http.Header{"Authorization": {"Bearer caller"}}), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "sdk-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listing, err := session.ListTools(ctx, nil)
	if err != nil || len(listing.Tools) != 4 {
		t.Fatalf("listing: %+v %v", listing, err)
	}
	for _, call := range []struct {
		name      string
		arguments map[string]any
	}{
		{"readItem", map[string]any{"path": map[string]any{"id": "item"}}},
		{"createItem", map[string]any{"body": map[string]any{"name": "Alpha", "count": 1, "note": nil}}},
		{"deleteItem", map[string]any{"path": map[string]any{"id": "item"}}},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.arguments})
		if err != nil || result.IsError {
			t.Fatalf("%s: %+v %v", call.name, result, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("REST calls: %d", calls.Load())
	}
}

func TestExecutorPreservesLargeJSONIntegers(t *testing.T) {
	var body map[string]json.RawMessage
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"integer":9007199254740993}`)
	}))
	defer backend.Close()
	operation := operationNamed(t, catalogFor(t, backend, adapterSchema), "createItem")
	result := Execute(context.Background(), backend.Client(), operation, json.RawMessage(`{"body":{"name":"Alpha","count":9007199254740993}}`), nil)
	encoded, _ := json.Marshal(result.StructuredContent)
	if result.IsError || string(body["count"]) != "9007199254740993" || !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatalf("integer changed: request=%s response=%s", body["count"], encoded)
	}
}
