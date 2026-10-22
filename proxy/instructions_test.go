package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func initializeInstructions(t *testing.T, rootExtension string) string {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"}%s,"paths":{"/a":{"get":{"operationId":"get_a","responses":{"200":{"description":"OK"}}}}}}`, rootExtension)
	}))
	t.Cleanup(host.Close)
	server, err := NewServer(Config{SchemaURL: host.URL + "/root.json", AnonymousSchema: true})
	if err != nil {
		t.Fatal(err)
	}
	response := post(t, NewHTTPHandler(server), initializeBody, nil)
	var envelope struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return envelope.Result.Instructions
}

func TestInitializeServesRootInstructions(t *testing.T) {
	if got := initializeInstructions(t, `,"x-mcp":{"instructions":"Ask before choosing a region."}`); got != "Ask before choosing a region." {
		t.Fatalf("instructions: %q", got)
	}
	for _, extension := range []string{``, `,"x-mcp":{"instructions":7}`, `,"x-mcp":"text"`} {
		if got := initializeInstructions(t, extension); got != "" {
			t.Fatalf("%s: instructions %q", extension, got)
		}
	}
}
