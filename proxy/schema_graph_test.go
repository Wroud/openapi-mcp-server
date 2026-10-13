package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func schemaTestHost(t *testing.T, documents map[string]string) (*httptest.Server, map[string]int, *sync.Mutex) {
	t.Helper()
	counts := make(map[string]int)
	lock := new(sync.Mutex)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		counts[r.URL.Path]++
		lock.Unlock()
		if value, ok := documents[r.URL.Path]; ok {
			fmt.Fprint(w, value)
		} else {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)
	return host, counts, lock
}

func TestProductionShapedReferenceGraph(t *testing.T) {
	files := map[string]string{"/openapi.json": "root.json", "/api/pricing/openapi.json": "pricing.json", "/api/loader-support/openapi.json": "loader.json", "/api/bedrock-support/openapi.json": "bedrock.json", "/api/blog/openapi.json": "blog.json", "/api/server-brief/openapi.json": "brief.json"}
	documents := make(map[string]string)
	for path, file := range files {
		data, err := os.ReadFile("testdata/reference-graph/" + file)
		if err != nil {
			t.Fatal(err)
		}
		documents[path] = string(data)
	}
	host, counts, lock := schemaTestHost(t, documents)
	loader, err := NewSchemaLoader(host.URL+"/openapi.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Paths.Len() != 7 {
		t.Fatalf("root paths: %d", doc.Paths.Len())
	}
	ids := []string{}
	for _, item := range doc.Paths.Map() {
		for _, operation := range item.Operations() {
			ids = append(ids, operation.OperationID)
		}
	}
	slices.Sort(ids)
	expected := []string{"getBedrockRange", "getBlogPost", "getLoaderVersions", "getPricingCatalog", "getRecommendedRegion", "getServerBrief", "listBlogPosts"}
	if !slices.Equal(ids, expected) {
		t.Fatalf("operation IDs: %v", ids)
	}
	if doc.Servers[0].URL != "/" || doc.Paths.Value("/api/blog/posts/{slug}").Get == nil || doc.Paths.Value("/api/server-brief").Get == nil {
		t.Fatal("root paths or server base changed")
	}
	lock.Lock()
	defer lock.Unlock()
	if len(counts) != 6 {
		t.Fatalf("fetched documents: %v", counts)
	}
	for path, count := range counts {
		if count != 1 {
			t.Fatalf("duplicate fetch for %s: %d", path, count)
		}
	}
}

func TestReferencesRecursionAndUnionSemantics(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/items":{"$ref":"./leaf.json#/paths/~1items"}},"components":{"schemas":{"Admitted":{"$ref":"./types.json#/components/schemas/Nullable"}}}}`
	leaf := `{"openapi":"3.1.0","info":{"title":"Leaf","version":"1"},"servers":[{"url":"/api/leaf"}],"paths":{"/items":{"get":{"operationId":"items","responses":{"200":{"description":"Items","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Node"}}}}}}},"/hidden":{"get":{"operationId":"hidden","responses":{"200":{"description":"Hidden"}}}}},"components":{"schemas":{"Node":{"type":"object","properties":{"next":{"$ref":"#/components/schemas/Node"},"value":{"$ref":"./types.json#/components/schemas/Nullable"},"enum":{"$ref":"./types.json#/components/schemas/Container/properties/Odd~1Name~0T"}}}}}}`
	types := `{"openapi":"3.1.0","info":{"title":"Types","version":"1"},"components":{"schemas":{"Nullable":{"type":["string","null"]},"Container":{"type":"object","properties":{"Odd/Name~T":{"anyOf":[{"type":"integer"},{"type":"null"}]}}}}}}`
	host, _, _ := schemaTestHost(t, map[string]string{"/root.json": root, "/leaf.json": leaf, "/types.json": types})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Paths.Len() != 1 || doc.Paths.Value("/hidden") != nil {
		t.Fatal("leaf-only paths were admitted")
	}
	schema := doc.Paths.Value("/items").Get.Responses.Value("200").Value.Content["application/json"].Schema.Value
	if schema.Properties["next"].Value != schema {
		t.Fatal("recursive schema identity was lost")
	}
	nullable := schema.Properties["value"].Value
	if err := nullable.VisitJSON(nil); err != nil {
		t.Fatal(err)
	}
	if err := nullable.VisitJSON("value"); err != nil {
		t.Fatal(err)
	}
	if err := nullable.VisitJSON(12); err == nil {
		t.Fatal("nullable union accepted an integer")
	}
	union := schema.Properties["enum"].Value
	if err := union.VisitJSON(nil); err != nil {
		t.Fatal(err)
	}
	if err := union.VisitJSON(12); err != nil {
		t.Fatal(err)
	}
	if err := union.VisitJSON("wrong"); err == nil {
		t.Fatal("anyOf accepted a string")
	}
}

func TestReferenceConfinementAndDiagnostics(t *testing.T) {
	for _, ref := range []string{"https://outside.example/schema.json#/components/schemas/X", "file:///tmp/schema.json#/X", "http://user:secret@outside.example/schema.json#/X", "#/components/schemas/Missing", "#/components/schemas/Bad~2Token", "#anchor", "/leaf.json#/missing"} {
		t.Run(ref, func(t *testing.T) {
			root := fmt.Sprintf(`{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"$ref":%q}}}}`, ref)
			host, counts, lock := schemaTestHost(t, map[string]string{"/root.json": root, "/leaf.json": `{"openapi":"3.1.0","info":{"title":"Leaf","version":"1"}}`})
			loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loader.Load(context.Background(), nil); err == nil {
				t.Fatal("expected reference rejection")
			}
			lock.Lock()
			defer lock.Unlock()
			if strings.HasPrefix(ref, "https:") || strings.HasPrefix(ref, "file:") || strings.Contains(ref, "user:secret") {
				if len(counts) != 1 {
					t.Fatalf("unexpected fetches: %v", counts)
				}
			}
		})
	}
}

func TestUnadmittedTransitiveDocument(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"$ref":"/leaf.json#/components/schemas/X"}}}}`
	leaf := `{"openapi":"3.1.0","info":{"title":"Leaf","version":"1"},"components":{"schemas":{"X":{"$ref":"/extra.json#/components/schemas/X"}}}}`
	extra := `{"openapi":"3.1.0","info":{"title":"Extra","version":"1"},"components":{"schemas":{"X":{"type":"string"}}}}`
	host, counts, lock := schemaTestHost(t, map[string]string{"/root.json": root, "/leaf.json": leaf, "/extra.json": extra})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("unadmitted error: %v", err)
	}
	lock.Lock()
	count := counts["/extra.json"]
	lock.Unlock()
	if count != 0 {
		t.Fatal("unadmitted document was fetched")
	}
	loader, err = NewSchemaLoader(host.URL+"/root.json", SchemaOptions{AllowedURLs: []string{"/extra.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaBudgets(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"$ref":"/leaf.json#/components/schemas/X"}}}}`
	leaf := `{"openapi":"3.1.0","info":{"title":"Leaf","version":"1"},"components":{"schemas":{"X":{"type":"string"}}}}`
	for _, tc := range []struct {
		name   string
		limits SchemaLimits
	}{
		{"document bytes", SchemaLimits{DocumentBytes: 20}},
		{"total bytes", SchemaLimits{TotalBytes: int64(len(root) + len(leaf) - 1)}},
		{"documents", SchemaLimits{Documents: 1}},
		{"JSON depth", SchemaLimits{JSONDepth: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _, _ := schemaTestHost(t, map[string]string{"/root.json": root, "/leaf.json": leaf})
			loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{Limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loader.Load(context.Background(), nil); err == nil {
				t.Fatal("expected budget error")
			}
		})
	}
	chain := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"A":{"$ref":"#/components/schemas/B"},"B":{"$ref":"#/components/schemas/C"},"C":{"$ref":"#/components/schemas/D"},"D":{"type":"string"}}}}`
	host, _, _ := schemaTestHost(t, map[string]string{"/root.json": chain})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{Limits: SchemaLimits{ReferenceDepth: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(context.Background(), nil); err == nil {
		t.Fatal("expected reference depth error")
	}
}

func TestSchemaCredentialsFreshnessAndFailureIsolation(t *testing.T) {
	var mu sync.Mutex
	version := "1"
	failed := false
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current, outage := version, failed
		mu.Unlock()
		if r.URL.Path == "/leaf.json" {
			if outage {
				w.WriteHeader(503)
				return
			}
			fmt.Fprintf(w, `{"openapi":"3.1.0","info":{"title":"Leaf","version":%q},"components":{"schemas":{"X":{"type":"string","description":%q}}}}`, current, r.Header.Get("Authorization")+"|"+current)
			return
		}
		fmt.Fprint(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"$ref":"/leaf.json#/components/schemas/X"}}}}`)
	}))
	defer host.Close()
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			token := fmt.Sprint(i)
			doc, err := loader.Load(context.Background(), http.Header{"Authorization": {token}})
			if err != nil {
				t.Error(err)
				return
			}
			if doc.Components.Schemas["X"].Value.Description != token+"|1" {
				t.Error("cross-caller schema credentials")
			}
		}()
	}
	group.Wait()
	mu.Lock()
	failed = true
	mu.Unlock()
	if doc, err := loader.Load(context.Background(), nil); err == nil || doc != nil {
		t.Fatal("published a partial or stale document")
	}
	mu.Lock()
	failed = false
	version = "2"
	mu.Unlock()
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Components.Schemas["X"].Value.Description != "|2" {
		t.Fatal("anonymous request reused caller credentials")
	}
}

func TestSchemaIDAndLiteralReferenceData(t *testing.T) {
	for _, keyword := range []string{"$id", "$dynamicRef", "$anchor"} {
		t.Run(keyword, func(t *testing.T) {
			data := fmt.Sprintf(`{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"type":"string",%q:"https://outside.example/"}}}}`, keyword)
			host, _, _ := schemaTestHost(t, map[string]string{"/root.json": data})
			loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loader.Load(context.Background(), nil); err == nil || !strings.Contains(err.Error(), keyword) {
				t.Fatalf("unsupported keyword: %v", err)
			}
		})
	}
	literal := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"$id":"root.json","type":"object","example":{"$ref":"https://outside.example/"},"examples":[{"$ref":"https://outside.example/"}],"properties":{"value":{"type":"string"}}}}}}`
	host, counts, lock := schemaTestHost(t, map[string]string{"/root.json": literal})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(counts) != 1 {
		t.Fatal("followed example data as a schema reference")
	}
}

func TestSchemaTotalDeadline(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer host.Close()
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{Limits: SchemaLimits{Timeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(context.Background(), nil); err == nil {
		t.Fatal("expected total deadline error")
	}
}

func TestStandaloneSchemaDocument(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"X":{"$ref":"./shape.json"}}}}`
	shape := `{"type":"object","$defs":{"Item":{"type":["string","null"]}},"properties":{"value":{"$ref":"#/$defs/Item"}}}`
	host, _, _ := schemaTestHost(t, map[string]string{"/root.json": root, "/shape.json": shape})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["X"].Value.Properties["value"].Value.VisitJSON(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCrossDocumentRecursionAndOpenAPI30Nullable(t *testing.T) {
	root := `{"openapi":"3.0.3","info":{"title":"API","version":"1"},"paths":{},"components":{"schemas":{"Node":{"$ref":"/leaf.json#/components/schemas/Node"}}}}`
	leaf := `{"openapi":"3.0.3","info":{"title":"Leaf","version":"1"},"paths":{},"components":{"schemas":{"Node":{"type":"object","properties":{"next":{"$ref":"/root.json#/components/schemas/Node"},"nullable":{"type":"string","nullable":true}}}}}}`
	host, _, _ := schemaTestHost(t, map[string]string{"/root.json": root, "/leaf.json": leaf})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loader.Load(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	node := doc.Components.Schemas["Node"].Value
	if node.Properties["next"].Value != node {
		t.Fatal("cross-document recursion lost schema identity")
	}
	if !node.Properties["nullable"].Value.Nullable {
		t.Fatal("OpenAPI 3.0 nullable was lost")
	}
}

func TestFailedSourceDropsDependentPaths(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{
"/api/public":{"get":{"operationId":"getPublic","responses":{"200":{"description":"OK"}}}},
"/api/blog":{"$ref":"/blog.json#/paths/~1posts"},
"/api/servers":{"$ref":"/servers.json#/paths/~1servers"},
"/api/servers/{id}":{"$ref":"/servers.json#/paths/~1servers~1{id}"},
"/api/shapes":{"$ref":"/shapes.json#/paths/~1shapes"}}}`
	blog := `{"openapi":"3.1.0","info":{"title":"Blog","version":"1"},"paths":{"/posts":{"get":{"operationId":"listPosts","responses":{"200":{"description":"OK"}}}}}}`
	servers := `{"openapi":"3.1.0","info":{"title":"Servers","version":"1"},"paths":{"/servers":{"get":{"operationId":"listServers","responses":{"200":{"description":"OK"}}}},"/servers/{id}":{"get":{"operationId":"getServer","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"OK"}}}}}}`
	shapes := `{"openapi":"3.1.0","info":{"title":"Shapes","version":"1"},"paths":{"/shapes":{"get":{"operationId":"listShapes","responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"$ref":"/types.json#/components/schemas/Shape"}}}}}}}}}`
	types := `{"openapi":"3.1.0","info":{"title":"Types","version":"1"},"components":{"schemas":{"Shape":{"type":"string"}}}}`
	invalid := `{"openapi":"3.1.0","info":{"title":"Servers"},"paths":{"/servers":{"get":{"operationId":"listServers","responses":{"200":{"description":"OK"}}}},"/servers/{id}":{"get":{"operationId":"getServer","responses":{"200":{"description":"OK"}}}}}}`
	for _, tc := range []struct {
		name     string
		failing  string
		status   int
		body     string
		hang     bool
		expected []string
	}{
		{"servers 401", "/servers.json", 401, "", false, []string{"/api/blog", "/api/public", "/api/shapes"}},
		{"servers 500", "/servers.json", 500, "", false, []string{"/api/blog", "/api/public", "/api/shapes"}},
		{"servers timeout", "/servers.json", 0, "", true, []string{"/api/blog", "/api/public", "/api/shapes"}},
		{"servers invalid JSON", "/servers.json", 200, "{", false, []string{"/api/blog", "/api/public", "/api/shapes"}},
		{"servers invalid document", "/servers.json", 200, invalid, false, []string{"/api/blog", "/api/public", "/api/shapes"}},
		{"transitive types", "/types.json", 503, "", false, []string{"/api/blog", "/api/public", "/api/servers", "/api/servers/{id}"}},
		{"servers stale pointer", "/servers.json", 200, `{"openapi":"3.1.0","info":{"title":"Servers","version":"1"},"paths":{"/servers":{"get":{"operationId":"listServers","responses":{"200":{"description":"OK"}}}}}}`, false, []string{"/api/blog", "/api/public", "/api/servers", "/api/shapes"}},
		{"servers oversized", "/servers.json", 200, `{"pad":"` + strings.Repeat("x", 5000) + `"}`, false, []string{"/api/blog", "/api/public", "/api/shapes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			documents := map[string]string{"/root.json": root, "/blog.json": blog, "/servers.json": servers, "/shapes.json": shapes, "/types.json": types}
			var lock sync.Mutex
			counts := make(map[string]int)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lock.Lock()
				counts[r.URL.Path]++
				lock.Unlock()
				if r.URL.Path == tc.failing {
					if tc.hang {
						<-r.Context().Done()
						return
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, documents[r.URL.Path])
			}))
			defer host.Close()
			loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{AllowedURLs: []string{"/types.json"}, Limits: SchemaLimits{Timeout: 2 * time.Second, SourceTimeout: 200 * time.Millisecond, DocumentBytes: 4096}})
			if err != nil {
				t.Fatal(err)
			}
			doc, err := loader.Load(context.Background(), http.Header{"Authorization": {"Bearer secret"}})
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{}
			for path := range doc.Paths.Map() {
				paths = append(paths, path)
			}
			slices.Sort(paths)
			if !slices.Equal(paths, tc.expected) {
				t.Fatalf("paths: %v", paths)
			}
			lock.Lock()
			defer lock.Unlock()
			for path, count := range counts {
				if count != 1 {
					t.Fatalf("duplicate fetch for %s: %d", path, count)
				}
			}
		})
	}
}

func TestFailedSourceStillFailsSharedComponents(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/api/servers":{"$ref":"/servers.json#/paths/~1servers"}},"components":{"schemas":{"X":{"$ref":"/servers.json#/components/schemas/X"}}}}`
	host, _, _ := schemaTestHost(t, map[string]string{"/root.json": root})
	loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Load(context.Background(), nil); err == nil {
		t.Fatal("a source referenced outside Path Items did not fail the load")
	}
}

func TestDegradedProxyServesRemainingTools(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{
"/api/public":{"get":{"operationId":"getPublic","responses":{"200":{"description":"OK"}}}},
"/api/servers":{"$ref":"/servers.json#/paths/~1servers"}}}`
	var backend atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/root.json":
			fmt.Fprint(w, root)
		case "/servers.json":
			w.WriteHeader(401)
		default:
			backend.Add(1)
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer host.Close()
	var buffer bytes.Buffer
	var lock sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&buffer, &lock}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server, err := NewServer(Config{SchemaURL: host.URL + "/root.json"})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(server)
	if result := rpc(t, handler, "initialize", nil); result["error"] != nil {
		t.Fatalf("initialize: %s", result)
	}
	listing := rpc(t, handler, "tools/list", http.Header{"Authorization": {"Bearer secret-token"}})
	if listing["error"] != nil || !strings.Contains(string(listing["result"]), "getPublic") || strings.Contains(string(listing["result"]), "listServers") {
		t.Fatalf("listing: %s", listing)
	}
	result := decodeResult(t, post(t, handler, callBody("getPublic"), nil))
	if result.IsError || backend.Load() != 1 {
		t.Fatalf("public call: %+v", result)
	}
	lock.Lock()
	logged := buffer.String()
	lock.Unlock()
	if !strings.Contains(logged, `"metric_name":"UnhandledHttpError"`) || !strings.Contains(logged, "/servers.json") || strings.Contains(logged, "secret-token") {
		t.Fatalf("degradation log: %s", logged)
	}
}
