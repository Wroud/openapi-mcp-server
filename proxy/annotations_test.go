package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const annotationSchema = `{"openapi":"3.1.0","info":{"title":"Annotated","version":"1"},"paths":{
"/items/{id}":{"parameters":[{"in":"path","name":"id","required":true,"schema":{"type":"string"}}],
"get":{"operationId":"readItem","summary":"Read item","description":"Read one item","responses":{"200":{"description":"ok"}}},
"head":{"operationId":"headItem","responses":{"200":{"description":"ok"}}},
"put":{"operationId":"replaceItem","x-mcp":{"annotations":{"destructiveHint":false}},"responses":{"200":{"description":"ok"}}},
"delete":{"operationId":"deleteItem","responses":{"204":{"description":"ok"}}}},
"/items":{"post":{"operationId":"createItem","summary":"Create item","responses":{"200":{"description":"ok"}}}},
"/search":{"post":{"operationId":"searchItems","x-mcp":{"annotations":{"readOnlyHint":true,"idempotentHint":true,"openWorldHint":true}},"responses":{"200":{"description":"ok"}}}},
"/lookup":{"post":{"operationId":"lookupItems","x-mcp":{"annotations":{"readOnlyHint":true}},"responses":{"200":{"description":"ok"}}}},
"/touch":{"get":{"operationId":"touchItems","x-mcp":{"annotations":{"readOnlyHint":false}},"responses":{"200":{"description":"ok"}}}},
"/invalid/case":{"post":{"operationId":"wrongCaseObject","x-mcp":{"Annotations":{"readOnlyHint":true}},"responses":{"200":{"description":"ok"}}}},
"/invalid/hint-case":{"post":{"operationId":"wrongCaseHint","x-mcp":{"annotations":{"readonlyhint":true}},"responses":{"200":{"description":"ok"}}}},
"/invalid/scalar":{"post":{"operationId":"scalarExtension","x-mcp":{"annotations":true},"responses":{"200":{"description":"ok"}}}},
"/invalid/extra":{"post":{"operationId":"extraKey","x-mcp":{"annotations":{},"name":"x"},"responses":{"200":{"description":"ok"}}}},
"/invalid/unknown":{"post":{"operationId":"unknownKey","x-mcp":{"annotations":{"title":"x"}},"responses":{"200":{"description":"ok"}}}},
"/invalid/type":{"post":{"operationId":"nonBoolean","x-mcp":{"annotations":{"readOnlyHint":"yes"}},"responses":{"200":{"description":"ok"}}}},
"/invalid/conflict":{"post":{"operationId":"readOnlyDestructive","x-mcp":{"annotations":{"readOnlyHint":true,"destructiveHint":true}},"responses":{"200":{"description":"ok"}}}}}}`

type wantAnnotations struct {
	readOnly, destructive, idempotent, openWorld bool
}

func assertAnnotations(t *testing.T, tool *mcp.Tool, want wantAnnotations) {
	t.Helper()
	annotations := tool.Annotations
	if annotations == nil || annotations.DestructiveHint == nil || annotations.OpenWorldHint == nil {
		t.Fatalf("%s: hints not explicit: %+v", tool.Name, annotations)
	}
	got := wantAnnotations{annotations.ReadOnlyHint, *annotations.DestructiveHint, annotations.IdempotentHint, *annotations.OpenWorldHint}
	if got != want {
		t.Fatalf("%s: got %+v want %+v", tool.Name, got, want)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
		if _, ok := decoded.Annotations[key].(bool); !ok {
			t.Fatalf("%s: %s missing from wire form: %s", tool.Name, key, encoded)
		}
	}
}

func TestToolAnnotationDefaultsAndOverrides(t *testing.T) {
	catalog := catalogFor(t, nil, annotationSchema)
	for name, want := range map[string]wantAnnotations{
		"readItem":    {readOnly: true, idempotent: true},
		"headItem":    {readOnly: true, idempotent: true},
		"createItem":  {destructive: true},
		"deleteItem":  {destructive: true, idempotent: true},
		"replaceItem": {idempotent: true},
		"searchItems": {readOnly: true, idempotent: true, openWorld: true},
		"lookupItems": {readOnly: true},
		"touchItems":  {destructive: true, idempotent: true},
	} {
		assertAnnotations(t, operationNamed(t, catalog, name).Tool, want)
	}
	skipped := map[string]string{}
	for _, operation := range catalog.Skipped {
		skipped[operation.Name] = operation.Reason
	}
	for _, name := range []string{"wrongCaseObject", "wrongCaseHint", "scalarExtension", "extraKey", "unknownKey", "nonBoolean", "readOnlyDestructive"} {
		if !strings.HasPrefix(skipped[name], "invalid x-mcp") {
			t.Fatalf("%s: reason %q", name, skipped[name])
		}
	}
	if len(catalog.Operations) != 8 || len(catalog.Skipped) != 7 {
		t.Fatalf("catalog: %d operations, skipped %+v", len(catalog.Operations), catalog.Skipped)
	}
}

func TestToolTitleFromSummary(t *testing.T) {
	catalog := catalogFor(t, nil, annotationSchema)
	read := operationNamed(t, catalog, "readItem").Tool
	if read.Title != "Read item" || read.Annotations.Title != "Read item" || read.Description != "Read one item" {
		t.Fatalf("readItem: %q %q %q", read.Title, read.Annotations.Title, read.Description)
	}
	create := operationNamed(t, catalog, "createItem").Tool
	if create.Title != "Create item" || create.Annotations.Title != "Create item" || create.Description != "Create item" {
		t.Fatalf("createItem: %q %q %q", create.Title, create.Annotations.Title, create.Description)
	}
	remove := operationNamed(t, catalog, "deleteItem").Tool
	if remove.Title != "" || remove.Description != "DELETE /items/{id}" {
		t.Fatalf("deleteItem: %q %q", remove.Title, remove.Description)
	}
}

func TestAPIInformationAnnotations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "info": APIInfo{Title: "API", Version: "1"}})
	}))
	defer upstream.Close()
	server, err := NewServer(Config{SchemaURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "annotations", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	information := tools.Tools[0]
	if information.Name != "get_api_information" || information.Title != "Get API information" || information.Annotations == nil || information.Annotations.Title != "Get API information" {
		t.Fatalf("information tool: %+v", information)
	}
	assertAnnotations(t, information, wantAnnotations{readOnly: true, idempotent: true})
}
