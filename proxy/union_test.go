package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const unionSchema = `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"servers":[{"url":"/v1"}],"paths":{"/servers":{"post":{"operationId":"createServer","requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object","required":["projects"],"properties":{"projects":{"type":"array","items":{"anyOf":[{"type":"string","minLength":1},{"type":"object","required":["coord"],"properties":{"coord":{"type":"string"},"settings":{"type":"object","additionalProperties":{"anyOf":[{"type":"string"},{"type":"number"},{"type":"boolean"}]}}}}]}}}}}}},"responses":{"200":{"description":"ok"}}}},"/pack":{"put":{"operationId":"setPack","requestBody":{"required":true,"content":{"application/json":{"schema":{"oneOf":[{"type":"object","required":["from","projectId"],"properties":{"from":{"type":"string","enum":["modrinth"]},"projectId":{"type":"string"}}},{"type":"object","required":["from","url"],"properties":{"from":{"type":"string","enum":["link"]},"url":{"type":"string"}}}]}}}},"responses":{"200":{"description":"ok"}}}},"/mixed":{"post":{"operationId":"mixed","requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"a":{"type":"string"}},"anyOf":[{"required":["a"]}]}}}},"responses":{"200":{"description":"ok"}}}},"/nested":{"post":{"operationId":"nested","requestBody":{"content":{"application/json":{"schema":{"anyOf":[{"allOf":[{"type":"string"}]}]}}}},"responses":{"200":{"description":"ok"}}}}}}`

func TestUnionInputSchemas(t *testing.T) {
	var received []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		received = append(received, r.URL.Path+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer backend.Close()
	catalog := catalogFor(t, backend, unionSchema)

	create := operationNamed(t, catalog, "createServer")
	encoded, _ := json.Marshal(create.Tool.InputSchema)
	if !strings.Contains(string(encoded), `"anyOf":[{"minLength":1,"type":"string"}`) {
		t.Fatalf("createServer input: %s", encoded)
	}
	pack := operationNamed(t, catalog, "setPack")
	encoded, _ = json.Marshal(pack.Tool.InputSchema)
	if !strings.Contains(string(encoded), `"body":{"oneOf":[`) {
		t.Fatalf("setPack input: %s", encoded)
	}
	skipped := map[string]bool{}
	for _, operation := range catalog.Skipped {
		skipped[operation.Name] = true
	}
	if !skipped["mixed"] || !skipped["nested"] || len(catalog.Operations) != 2 {
		t.Fatalf("catalog: %+v", catalog)
	}

	if result := Execute(context.Background(), backend.Client(), create, json.RawMessage(`{"body":{"projects":["fabric-api",{"coord":"sodium","settings":{"x":1}}]}}`), nil); result.IsError {
		t.Fatalf("valid union rejected: %+v", result.Content)
	}
	if result := Execute(context.Background(), backend.Client(), pack, json.RawMessage(`{"body":{"from":"link","url":"https://example.test/p.zip"}}`), nil); result.IsError {
		t.Fatalf("valid oneOf rejected: %+v", result.Content)
	}
	if result := Execute(context.Background(), backend.Client(), pack, json.RawMessage(`{"body":{"from":"link","projectId":"x"}}`), nil); !result.IsError {
		t.Fatal("invalid oneOf accepted")
	}
	if len(received) != 2 {
		t.Fatalf("backend requests: %v", received)
	}
}

func TestNullableUnionInOpenAPI30(t *testing.T) {
	schema := `{"openapi":"3.0.3","info":{"title":"API","version":"1"},"servers":[{"url":"/v1"}],"paths":{"/value":{"put":{"operationId":"setValue","requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"integer"}],"nullable":true}}}}}},"responses":{"200":{"description":"ok"}}}}}}`
	encoded, _ := json.Marshal(operationNamed(t, catalogFor(t, nil, schema), "setValue").Tool.InputSchema)
	if strings.Contains(string(encoded), `""`) || !strings.Contains(string(encoded), `"anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}]`) {
		t.Fatalf("input: %s", encoded)
	}
}
