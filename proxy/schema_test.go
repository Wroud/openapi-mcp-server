package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSchemaSourceFailurePreventsMetadata(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/leaf.json" {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"paths":{"/items":{"$ref":"/leaf.json#/paths/~1items"}},"components":{"schemas":{"Item":{"$ref":"/leaf.json#/components/schemas/Item"}}}}`)
	}))
	defer source.Close()
	handler, err := NewHandler(source.URL + "/root.json")
	if err != nil {
		t.Fatal(err)
	}
	result := rpc(t, handler, "tools/call", nil)
	var output struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result["result"], &output); err != nil {
		t.Fatal(err)
	}
	if !output.IsError {
		t.Fatal("metadata succeeded with an unresolved schema source")
	}
}
