package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/wroud/openapi-mcp-server/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	loader, err := proxy.NewSchemaLoader(os.Getenv("OPENAPI_URL"), proxy.SchemaOptions{})
	if err != nil {
		return err
	}
	document, err := loader.Load(context.Background(), nil)
	if err != nil {
		return err
	}
	paths := make(map[string]map[string]string)
	if document.Paths != nil {
		for path, item := range document.Paths.Map() {
			methods := make(map[string]string)
			for method, operation := range item.Operations() {
				methods[method] = operation.OperationID
			}
			paths[path] = methods
		}
	}
	catalog, err := proxy.BuildCatalog(document, os.Getenv("OPENAPI_URL"), os.Getenv("BACKEND_URL"))
	if err != nil {
		return err
	}
	supported := []string{}
	for _, operation := range catalog.Operations {
		supported = append(supported, operation.Tool.Name)
	}
	output := struct {
		API       proxy.APIInfo                `json:"api"`
		Paths     map[string]map[string]string `json:"paths"`
		Supported []string                     `json:"supported"`
		Skipped   []proxy.SkippedOperation     `json:"skipped"`
	}{API: proxy.APIInfo{Title: document.Info.Title, Version: document.Info.Version, Description: document.Info.Description}, Paths: paths, Supported: supported, Skipped: catalog.Skipped}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
