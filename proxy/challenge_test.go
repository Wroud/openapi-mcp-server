package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testMetadataURL = "https://mcp.example/.well-known/oauth-protected-resource"
	missingHeader   = `Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource", scope="pixly"`
	missingMeta     = `Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource", scope="pixly", error="invalid_token", error_description="Sign in to use this tool"`
	rejectedHeader  = `Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource", scope="pixly", error="invalid_token", error_description="The access token is invalid or expired"`
	chatGPTAgent    = "openai-mcp/1.0.0"
)

const protectedDocument = `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"security":[{"bearer":[]}],
"components":{"securitySchemes":{"bearer":{"type":"http","scheme":"bearer"}}},
"paths":{
"/api/public":{"get":{"operationId":"getPublic","security":[],"responses":{"200":{"description":"OK"}}}},
"/api/private":{"get":{"operationId":"getPrivate","responses":{"200":{"description":"OK"}}}}}}`

type protectedFixture struct {
	handler        http.Handler
	backendHits    atomic.Int32
	status         atomic.Int32
	schemaAuth     sync.Map
	backendAuth    sync.Map
	schemaRequests atomic.Int32
}

func newProtectedFixture(t *testing.T, config Config) *protectedFixture {
	t.Helper()
	fixture := &protectedFixture{}
	fixture.status.Store(200)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			fixture.schemaRequests.Add(1)
			if value := r.Header.Get("Authorization"); value != "" {
				fixture.schemaAuth.Store(value, true)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, protectedDocument)
			return
		}
		fixture.backendHits.Add(1)
		if value := r.Header.Get("Authorization"); value != "" {
			fixture.backendAuth.Store(value, true)
		}
		status := int(fixture.status.Load())
		if r.URL.Path == "/api/public" && status == 200 {
			fmt.Fprint(w, `{"public":true}`)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	config.SchemaURL = upstream.URL + "/openapi.json"
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = NewHTTPHandler(server)
	return fixture
}

func challengeConfig() Config {
	return Config{ResourceMetadataURL: testMetadataURL, ChallengeScope: "pixly", ToolResultChallengeUserAgents: []string{"OpenAI-MCP"}}
}

func post(t *testing.T, handler http.Handler, body string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	for name, values := range headers {
		request.Header[name] = values
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-03-26")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func callBody(name string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, name)
}

type toolResult struct {
	IsError bool           `json:"isError"`
	Meta    map[string]any `json:"_meta"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func decodeResult(t *testing.T, response *httptest.ResponseRecorder) toolResult {
	t.Helper()
	if response.Code != 200 {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result toolResult      `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return envelope.Result
}

func assertHTTPChallenge(t *testing.T, response *httptest.ResponseRecorder, challenge string) {
	t.Helper()
	if response.Code != 401 {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != challenge {
		t.Fatalf("WWW-Authenticate: %q", got)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control: %q", response.Header().Get("Cache-Control"))
	}
	if strings.Contains(response.Body.String(), "jsonrpc") {
		t.Fatalf("challenge kept the buffered body: %s", response.Body.String())
	}
}

func assertMetaChallenge(t *testing.T, result toolResult, challenge string) {
	t.Helper()
	values, _ := result.Meta["mcp/www_authenticate"].([]any)
	if !result.IsError || len(values) != 1 || values[0] != challenge || len(result.Content) == 0 {
		t.Fatalf("tool-result challenge: %+v", result)
	}
}

func TestMissingAuthorizationChallenges(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	assertHTTPChallenge(t, post(t, fixture.handler, callBody("getPrivate"), nil), missingHeader)
	result := decodeResult(t, post(t, fixture.handler, callBody("getPrivate"), http.Header{"User-Agent": {chatGPTAgent}}))
	assertMetaChallenge(t, result, missingMeta)
	if fixture.backendHits.Load() != 0 {
		t.Fatalf("backend called without Authorization: %d", fixture.backendHits.Load())
	}
	public := decodeResult(t, post(t, fixture.handler, callBody("getPublic"), nil))
	if public.IsError || public.Meta != nil || fixture.backendHits.Load() != 1 {
		t.Fatalf("public call: %+v", public)
	}
	info := decodeResult(t, post(t, fixture.handler, callBody("get_api_information"), nil))
	if info.IsError {
		t.Fatalf("get_api_information: %+v", info)
	}
}

func TestRejectedTokenChallenges(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	fixture.status.Store(401)
	token := http.Header{"Authorization": {"Bearer stale-token"}}
	response := post(t, fixture.handler, callBody("getPrivate"), token)
	assertHTTPChallenge(t, response, rejectedHeader)
	if strings.Contains(response.Header().Get("WWW-Authenticate"), "stale-token") {
		t.Fatal("challenge echoed the token")
	}
	matched := http.Header{"Authorization": {"Bearer stale-token"}, "User-Agent": {chatGPTAgent}}
	assertMetaChallenge(t, decodeResult(t, post(t, fixture.handler, callBody("getPrivate"), matched)), rejectedHeader)
	if fixture.backendHits.Load() != 2 {
		t.Fatalf("backend hits: %d", fixture.backendHits.Load())
	}
	public := decodeResult(t, post(t, fixture.handler, callBody("getPublic"), token))
	if !public.IsError || public.Meta != nil || !strings.HasPrefix(public.Content[0].Text, "HTTP 401") {
		t.Fatalf("public 401: %+v", public)
	}
	for _, status := range []int32{403, 404, 500} {
		fixture.status.Store(status)
		for _, headers := range []http.Header{token, matched} {
			result := decodeResult(t, post(t, fixture.handler, callBody("getPrivate"), headers))
			if !result.IsError || result.Meta != nil || !strings.HasPrefix(result.Content[0].Text, fmt.Sprintf("HTTP %d", status)) {
				t.Fatalf("status %d: %+v", status, result)
			}
		}
	}
}

func TestChallengesDisabledWithoutResourceMetadata(t *testing.T) {
	fixture := newProtectedFixture(t, Config{ChallengeScope: "pixly", ToolResultChallengeUserAgents: []string{"OpenAI-MCP"}})
	fixture.status.Store(401)
	for _, headers := range []http.Header{nil, {"Authorization": {"Bearer stale"}}, {"User-Agent": {chatGPTAgent}}} {
		result := decodeResult(t, post(t, fixture.handler, callBody("getPrivate"), headers))
		if !result.IsError || result.Meta != nil || !strings.HasPrefix(result.Content[0].Text, "HTTP 401") {
			t.Fatalf("disabled challenge: %+v", result)
		}
	}
	if fixture.backendHits.Load() != 3 {
		t.Fatalf("backend hits: %d", fixture.backendHits.Load())
	}
	listing := post(t, fixture.handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if listing.Code != 200 || strings.Contains(listing.Body.String(), "securitySchemes") {
		t.Fatalf("listing: %d %s", listing.Code, listing.Body.String())
	}
}

func TestToolsListSecuritySchemes(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	response := post(t, fixture.handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if response.Code != 200 {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"get_api_information": `[{"type":"noauth"}]`,
		"getPublic":           `[{"type":"noauth"}]`,
		"getPrivate":          `[{"type":"oauth2","scopes":["pixly"]}]`,
	}
	if len(envelope.Result.Tools) != len(expected) {
		t.Fatalf("tools: %s", response.Body.String())
	}
	for _, tool := range envelope.Result.Tools {
		var name string
		json.Unmarshal(tool["name"], &name)
		var meta map[string]json.RawMessage
		json.Unmarshal(tool["_meta"], &meta)
		if string(tool["securitySchemes"]) != expected[name] || string(meta["securitySchemes"]) != expected[name] {
			t.Fatalf("%s: top-level %s, _meta %s", name, tool["securitySchemes"], meta["securitySchemes"])
		}
	}
	unscoped := newProtectedFixture(t, Config{ResourceMetadataURL: testMetadataURL})
	body := post(t, unscoped.handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil).Body.String()
	if !strings.Contains(body, `"securitySchemes":[{"type":"oauth2","scopes":[]}]`) {
		t.Fatalf("unscoped listing: %s", body)
	}
	assertHTTPChallenge(t, post(t, unscoped.handler, callBody("getPrivate"), nil), `Bearer resource_metadata="`+testMetadataURL+`"`)
}

func TestBatchIsNotBuffered(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	response := post(t, fixture.handler, "["+callBody("getPrivate")+"]", nil)
	if response.Code != 200 || response.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("batch: %d %s", response.Code, response.Body.String())
	}
	var batch []struct {
		Result toolResult `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil || len(batch) != 1 {
		t.Fatalf("batch body: %s", response.Body.String())
	}
	if !batch[0].Result.IsError || batch[0].Result.Meta != nil || fixture.backendHits.Load() != 0 {
		t.Fatalf("batch result: %+v", batch[0].Result)
	}
}

func TestConcurrentChallengeHolders(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	var group sync.WaitGroup
	for i := 0; i < 48; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if i%2 == 0 {
				response := post(t, fixture.handler, callBody("getPrivate"), nil)
				if response.Code != 401 {
					t.Errorf("protected call %d: HTTP %d", i, response.Code)
				}
				return
			}
			response := post(t, fixture.handler, callBody("getPublic"), nil)
			if response.Code != 200 || response.Header().Get("WWW-Authenticate") != "" {
				t.Errorf("public call %d: HTTP %d", i, response.Code)
			}
		}()
	}
	group.Wait()
}

func TestAnonymousSchemaLoads(t *testing.T) {
	config := challengeConfig()
	config.AnonymousSchema = true
	fixture := newProtectedFixture(t, config)
	token := http.Header{"Authorization": {"Bearer alice"}}
	if response := post(t, fixture.handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, token); response.Code != 200 {
		t.Fatalf("listing: %d", response.Code)
	}
	if result := decodeResult(t, post(t, fixture.handler, callBody("getPrivate"), token)); result.IsError {
		t.Fatalf("protected call: %+v", result)
	}
	if result := decodeResult(t, post(t, fixture.handler, callBody("get_api_information"), token)); result.IsError {
		t.Fatalf("information: %+v", result)
	}
	if _, seen := fixture.backendAuth.Load("Bearer alice"); !seen {
		t.Fatal("backend did not receive the caller credentials")
	}
	fixture.schemaAuth.Range(func(key, _ any) bool {
		t.Fatalf("schema received credentials: %v", key)
		return false
	})
	credentialed := newProtectedFixture(t, challengeConfig())
	post(t, credentialed.handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, token)
	if _, seen := credentialed.schemaAuth.Load("Bearer alice"); !seen {
		t.Fatal("default schema loads stopped forwarding credentials")
	}
}

func TestInitializeLogsClientWithoutCredentials(t *testing.T) {
	var buffer bytes.Buffer
	var lock sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&buffer, &lock}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	fixture := newProtectedFixture(t, challengeConfig())
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-ai","version":"0.1.0"}}}`
	response := post(t, fixture.handler, body, http.Header{"Authorization": {"Bearer secret-token"}, "Cookie": {"session=secret-cookie"}, "User-Agent": {"Claude-User/1.0"}})
	if response.Code != 200 {
		t.Fatalf("initialize: %d", response.Code)
	}
	lock.Lock()
	logged := buffer.String()
	lock.Unlock()
	if !strings.Contains(logged, `"user_agent":"Claude-User/1.0"`) || !strings.Contains(logged, `"client_name":"claude-ai"`) || !strings.Contains(logged, `"client_version":"0.1.0"`) {
		t.Fatalf("client log: %s", logged)
	}
	if strings.Contains(logged, "secret-token") || strings.Contains(logged, "secret-cookie") {
		t.Fatalf("credentials logged: %s", logged)
	}
}

type lockedWriter struct {
	buffer *bytes.Buffer
	lock   *sync.Mutex
}

func (w lockedWriter) Write(data []byte) (int, error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.buffer.Write(data)
}

func TestProtectedClassification(t *testing.T) {
	document := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"security":[{"bearer":[]}],
"components":{"securitySchemes":{"bearer":{"type":"http","scheme":"bearer"}}},
"paths":{
"/inherited":{"get":{"operationId":"inherited","responses":{"200":{"description":"OK"}}}},
"/public":{"get":{"operationId":"public","security":[],"responses":{"200":{"description":"OK"}}}},
"/optional":{"get":{"operationId":"optional","security":[{},{"bearer":[]}],"responses":{"200":{"description":"OK"}}}},
"/declared":{"get":{"operationId":"declared","security":[{"bearer":[]}],"responses":{"200":{"description":"OK"}}}}}}`
	open := strings.Replace(document, `"security":[{"bearer":[]}],
"components"`, `"security":[{"bearer":[]},{}],
"components"`, 1)
	for _, tc := range []struct {
		document string
		expected map[string]bool
	}{
		{document, map[string]bool{"inherited": true, "public": false, "optional": false, "declared": true}},
		{open, map[string]bool{"inherited": false, "public": false, "optional": false, "declared": true}},
		{strings.Replace(document, `"security":[{"bearer":[]}],
"components"`, `"components"`, 1), map[string]bool{"inherited": false, "public": false, "optional": false, "declared": true}},
	} {
		host, _, _ := schemaTestHost(t, map[string]string{"/openapi.json": tc.document})
		loader, err := NewSchemaLoader(host.URL+"/openapi.json", SchemaOptions{})
		if err != nil {
			t.Fatal(err)
		}
		doc, err := loader.Load(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := BuildCatalog(doc, host.URL+"/openapi.json", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range catalog.Operations {
			if operation.Protected != tc.expected[operation.Tool.Name] {
				t.Fatalf("%s protected=%v", operation.Tool.Name, operation.Protected)
			}
		}
	}
}

func TestChallengesFollowConfiguredCredentialHeaders(t *testing.T) {
	config := challengeConfig()
	config.AuthHeaders = []string{"X-Api-Key"}
	fixture := newProtectedFixture(t, config)
	result := decodeResult(t, post(t, fixture.handler, callBody("getPrivate"), http.Header{"X-Api-Key": {"key"}}))
	if result.IsError || fixture.backendHits.Load() != 1 {
		t.Fatalf("forwarded credential challenged: %+v", result)
	}
	fixture.status.Store(401)
	assertHTTPChallenge(t, post(t, fixture.handler, callBody("getPrivate"), http.Header{"X-Api-Key": {"stale"}}), rejectedHeader)
	assertHTTPChallenge(t, post(t, fixture.handler, callBody("getPrivate"), http.Header{"Authorization": {"Bearer ignored"}}), missingHeader)
}

func TestChallengesAreLoggedWithoutCredentials(t *testing.T) {
	var buffer bytes.Buffer
	var lock sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&buffer, &lock}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	fixture := newProtectedFixture(t, challengeConfig())
	post(t, fixture.handler, callBody("getPrivate"), http.Header{"User-Agent": {"claude-code/2.1"}})
	fixture.status.Store(401)
	post(t, fixture.handler, callBody("getPrivate"), http.Header{"Authorization": {"Bearer secret-token"}, "User-Agent": {chatGPTAgent}})
	lock.Lock()
	logged := buffer.String()
	lock.Unlock()
	for _, expected := range []string{
		`"msg":"Sign-in challenge","tool":"getPrivate","reason":"missing","form":"http","user_agent":"claude-code/2.1","authorization_header":false`,
		`"msg":"Sign-in challenge","tool":"getPrivate","reason":"rejected","form":"tool_result","user_agent":"openai-mcp/1.0.0","authorization_header":true`,
	} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("missing %s in %s", expected, logged)
		}
	}
	if strings.Contains(logged, "secret-token") {
		t.Fatalf("token logged: %s", logged)
	}
}
