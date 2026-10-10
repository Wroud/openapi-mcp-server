package proxy

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizedReferenceBytesAreBounded(t *testing.T) {
	root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"A":{"type":"string"},"B":{"$ref":"#/components/schemas/A"},"C":{"$ref":"#/components/schemas/A"},"D":{"$ref":"#/components/schemas/A"}}}}`
	path := "/" + strings.Repeat("segment/", 32) + "root.json"
	host, _, _ := schemaTestHost(t, map[string]string{path: root})
	loader, err := NewSchemaLoader(host.URL+path, SchemaOptions{Limits: SchemaLimits{DocumentBytes: int64(len(root) + 50), TotalBytes: int64(len(root) + 50)}})
	if err != nil {
		t.Fatal(err)
	}
	if document, err := loader.Load(context.Background(), nil); err == nil || document != nil {
		t.Fatal("normalized reference graph exceeded the byte budget without rejection")
	}
}

func TestMalformedArrayReferenceIndexes(t *testing.T) {
	for _, index := range []string{"+0", "-0"} {
		t.Run(index, func(t *testing.T) {
			root := `{"openapi":"3.1.0","info":{"title":"API","version":"1"},"components":{"schemas":{"A":{"anyOf":[{"type":"string"}]},"B":{"$ref":"#/components/schemas/A/anyOf/` + index + `"}}}}`
			host, _, _ := schemaTestHost(t, map[string]string{"/root.json": root})
			loader, err := NewSchemaLoader(host.URL+"/root.json", SchemaOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if document, err := loader.Load(context.Background(), nil); err == nil || document != nil {
				t.Fatal("malformed JSON Pointer array index was accepted")
			}
		})
	}
}
