package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const parallelRoot = `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{
"/api/a":{"$ref":"/a.json#/paths/~1a"},
"/api/b":{"$ref":"/b.json#/paths/~1b"},
"/api/c":{"$ref":"/c.json#/paths/~1c"}}}`

func sourceDocument(name string) string {
	return fmt.Sprintf(`{"openapi":"3.1.0","info":{"title":%q,"version":"1"},"paths":{"/%s":{"get":{"operationId":"get_%s","responses":{"200":{"description":"OK"}}}}}}`, name, name, name)
}

func TestSourcesLoadInParallel(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/root.json" {
			fmt.Fprint(w, parallelRoot)
			return
		}
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, sourceDocument(r.URL.Path[1:2]))
	}))
	defer host.Close()
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{Limits: SchemaLimits{Timeout: 800 * time.Millisecond, SourceTimeout: 600 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for path := range doc.Paths.Map() {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/api/a", "/api/b", "/api/c"}) || time.Since(started) > 700*time.Millisecond {
		t.Fatalf("paths %v after %v", paths, time.Since(started))
	}
}

type cacheFixture struct {
	handler   http.Handler
	rootHits  atomic.Int32
	failing   atomic.Bool
	sourceHit atomic.Int32
}

func newCacheFixture(t *testing.T, ttl time.Duration) *cacheFixture {
	t.Helper()
	return newConfiguredCacheFixture(t, Config{AnonymousSchema: true, SchemaCacheTTL: ttl})
}

func newConfiguredCacheFixture(t *testing.T, config Config) *cacheFixture {
	t.Helper()
	fixture := &cacheFixture{}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/root.json":
			fixture.rootHits.Add(1)
			fmt.Fprint(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/api/a":{"$ref":"/a.json#/paths/~1a"},"/api/b":{"$ref":"/b.json#/paths/~1b"}}}`)
		case "/b.json":
			fixture.sourceHit.Add(1)
			if fixture.failing.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, sourceDocument("b"))
		default:
			fmt.Fprint(w, sourceDocument("a"))
		}
	}))
	t.Cleanup(host.Close)
	config.SchemaURL = host.URL + "/root.json"
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = NewHTTPHandler(server)
	return fixture
}

func listedTools(t *testing.T, handler http.Handler) []string {
	t.Helper()
	response := post(t, handler, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	names := []string{}
	for _, tool := range envelope.Result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestAnonymousSchemaCache(t *testing.T) {
	complete := []string{"get_a", "get_api_information", "get_b"}

	fixture := newCacheFixture(t, time.Hour)
	for range 3 {
		if names := listedTools(t, fixture.handler); !slices.Equal(names, complete) {
			t.Fatalf("tools: %v", names)
		}
	}
	if fixture.rootHits.Load() != 1 {
		t.Fatalf("cached schema refetched: %d", fixture.rootHits.Load())
	}

	uncached := newCacheFixture(t, 0)
	listedTools(t, uncached.handler)
	listedTools(t, uncached.handler)
	if uncached.rootHits.Load() != 2 {
		t.Fatalf("schema cached without a TTL: %d", uncached.rootHits.Load())
	}

	degraded := newCacheFixture(t, time.Hour)
	degraded.failing.Store(true)
	if names := listedTools(t, degraded.handler); !slices.Equal(names, []string{"get_a", "get_api_information"}) {
		t.Fatalf("degraded tools: %v", names)
	}
	degraded.failing.Store(false)
	if names := listedTools(t, degraded.handler); !slices.Equal(names, complete) {
		t.Fatalf("degraded schema was cached: %v", names)
	}

	expired := newCacheFixture(t, 50*time.Millisecond)
	listedTools(t, expired.handler)
	expired.failing.Store(true)
	time.Sleep(80 * time.Millisecond)
	if names := listedTools(t, expired.handler); !slices.Equal(names, complete) {
		t.Fatalf("a degraded refresh replaced the last complete schema: %v", names)
	}
	if expired.rootHits.Load() != 2 {
		t.Fatalf("expired schema not refreshed: %d", expired.rootHits.Load())
	}
}

func TestAnonymousSchemaCacheConcurrentCallers(t *testing.T) {
	fixture := newCacheFixture(t, time.Hour)
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			listedTools(t, fixture.handler)
		}()
	}
	wait.Wait()
	if fixture.rootHits.Load() != 1 {
		t.Fatalf("concurrent callers refetched: %d", fixture.rootHits.Load())
	}
}

func TestConfigFromEnvDurations(t *testing.T) {
	t.Setenv("SCHEMA_CACHE_TTL", "5m")
	t.Setenv("SCHEMA_TIMEOUT", "20s")
	t.Setenv("SCHEMA_SOURCE_TIMEOUT", "15s")
	config, err := ConfigFromEnv()
	if err != nil || config.SchemaCacheTTL != 5*time.Minute || config.SchemaOptions.Limits.Timeout != 20*time.Second || config.SchemaOptions.Limits.SourceTimeout != 15*time.Second {
		t.Fatalf("config %+v, %v", config, err)
	}
	t.Setenv("SCHEMA_SOURCE_TIMEOUT", "soon")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("invalid duration accepted")
	}
}

func TestDisableAPIInformationTool(t *testing.T) {
	config := Config{AnonymousSchema: true, SchemaCacheTTL: time.Hour, DisableAPIInformationTool: true}
	fixture := newConfiguredCacheFixture(t, config)
	if names := listedTools(t, fixture.handler); !slices.Equal(names, []string{"get_a", "get_b"}) {
		t.Fatalf("tools: %v", names)
	}
	unknown := post(t, fixture.handler, callBody("unknown_tool"), nil).Body.String()
	called := post(t, fixture.handler, callBody("get_api_information"), nil).Body.String()
	if !strings.Contains(unknown, `"error"`) || called != strings.ReplaceAll(unknown, "unknown_tool", "get_api_information") {
		t.Fatalf("disabled call %s, unknown call %s", called, unknown)
	}

	initialize := post(t, fixture.handler, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, nil)
	var initialized struct {
		Result struct {
			Capabilities struct {
				Tools *struct{} `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initialize.Body.Bytes(), &initialized); err != nil || initialized.Result.Capabilities.Tools == nil {
		t.Fatalf("initialize %s: %v", initialize.Body.String(), err)
	}

	degraded := newConfiguredCacheFixture(t, config)
	degraded.failing.Store(true)
	if names := listedTools(t, degraded.handler); !slices.Equal(names, []string{"get_a"}) {
		t.Fatalf("degraded tools: %v", names)
	}
}

func TestConfigFromEnvDisableAPIInformationTool(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, "true": true} {
		t.Setenv("DISABLE_API_INFORMATION_TOOL", value)
		config, err := ConfigFromEnv()
		if err != nil || config.DisableAPIInformationTool != want {
			t.Fatalf("%q: %+v, %v", value, config, err)
		}
	}
}
