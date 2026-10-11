package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Operation struct {
	Tool           *mcp.Tool
	Method         string
	Path           string
	Parameters     openapi3.Parameters
	Body           *openapi3.SchemaRef
	BodyRequired   bool
	Backend        *url.URL
	JSONSchema2020 bool
}

type SkippedOperation struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
type Catalog struct {
	Operations []Operation
	Skipped    []SkippedOperation
}

func BuildCatalog(document *openapi3.T, schemaURL, backendOverride string) (Catalog, error) {
	catalog := Catalog{Operations: []Operation{}, Skipped: []SkippedOperation{}}
	source, err := url.Parse(schemaURL)
	if err != nil {
		return catalog, err
	}
	backend := source.ResolveReference(&url.URL{Path: "/"})
	if backendOverride != "" {
		backend, err = url.Parse(backendOverride)
	} else if len(document.Servers) > 0 {
		server := document.Servers[0]
		address := server.URL
		for name, variable := range server.Variables {
			address = strings.ReplaceAll(address, "{"+name+"}", variable.Default)
		}
		var relative *url.URL
		relative, err = url.Parse(address)
		if err == nil {
			backend = source.ResolveReference(relative)
		}
	}
	if err != nil || !validBackend(backend) {
		return catalog, errors.New("backend must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if backendOverride == "" && !sameOrigin(source, backend) {
		return catalog, errors.New("root server is outside the schema origin; configure BackendURL explicitly")
	}
	names := map[string]bool{"get_api_information": true}
	paths := []string{}
	if document.Paths != nil {
		for path := range document.Paths.Map() {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	size := 0
	for _, path := range paths {
		item := document.Paths.Value(path)
		methods := []string{}
		for method := range item.Operations() {
			methods = append(methods, method)
		}
		slices.Sort(methods)
		for _, method := range methods {
			definition := item.Operations()[method]
			name := definition.OperationID
			if name == "" {
				name = strings.ToLower(method) + "_" + regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString(strings.Trim(path, "/"), "_")
			}
			if names[name] {
				return catalog, fmt.Errorf("tool name collision: %s", name)
			}
			names[name] = true
			operation := Operation{Method: method, Path: path, Backend: backend, JSONSchema2020: document.IsOpenAPI31OrLater()}
			reason := ""
			if !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`).MatchString(name) {
				reason = "unsupported tool name"
			}
			if backendOverride == "" && (len(item.Servers) > 0 || definition.Servers != nil && len(*definition.Servers) > 0) {
				reason = "operation server override requires explicit BackendURL"
			}
			if strings.ContainsAny(path, "%\\?#") || unsafeSegments(path) {
				reason = "unsafe operation path"
			}
			switch method {
			case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				reason = "unsupported HTTP method"
			}
			if document.JSONSchemaDialect != "" && document.JSONSchemaDialect != "https://json-schema.org/draft/2020-12/schema" && document.JSONSchemaDialect != "https://spec.openapis.org/oas/3.1/dialect/base" {
				reason = "unsupported JSON Schema dialect"
			}
			parameters := make(map[string]*openapi3.ParameterRef)
			for _, ref := range append(append(openapi3.Parameters{}, item.Parameters...), definition.Parameters...) {
				parameters[ref.Value.In+":"+ref.Value.Name] = ref
			}
			keys := []string{}
			for key := range parameters {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			groups := map[string]map[string]any{"path": objectSchema(), "query": objectSchema()}
			budget := 0
			for _, key := range keys {
				ref := parameters[key]
				parameter := ref.Value
				if parameter.In != "path" && parameter.In != "query" {
					reason = "header and cookie input parameters are not supported"
					break
				}
				if parameter.Schema == nil || parameter.Schema.Value == nil || len(parameter.Content) > 0 || parameter.AllowReserved || parameter.AllowEmptyValue {
					reason = "unsupported parameter format"
					break
				}
				schema := parameter.Schema.Value
				kind, kindErr := inputKind(schema)
				if kindErr != nil {
					reason = kindErr.Error()
					break
				}
				if parameter.In == "path" {
					if kind == "object" || kind == "array" || kind == "" || kind == "null" || schema.Nullable || schema.Type.Includes("null") || parameter.Style != "" && parameter.Style != "simple" || parameter.Explode != nil && *parameter.Explode {
						reason = "only non-null scalar simple path parameters are supported"
						break
					}
				} else {
					if kind == "object" || kind == "" || parameter.Style != "" && parameter.Style != "form" || kind == "array" && (parameter.Explode != nil && !*parameter.Explode || schema.Items == nil || schema.Items.Value == nil) {
						reason = "only scalar or repeated-key form query parameters are supported"
						break
					}
					if kind == "array" {
						itemKind, itemErr := inputKind(schema.Items.Value)
						if itemErr != nil || itemKind == "array" || itemKind == "object" || itemKind == "" || itemKind == "null" || schema.Items.Value.Nullable || schema.Items.Value.Type.Includes("null") {
							reason = "query arrays must contain non-null scalars"
							break
						}
					}
					if schema.Nullable || schema.Type.Includes("null") {
						reason = "nullable query parameters are not supported"
						break
					}
				}
				input, conversionErr := inputSchema(schema, operation.JSONSchema2020, make(map[*openapi3.Schema]bool), 0, &budget)
				if conversionErr != nil {
					reason = conversionErr.Error()
					break
				}
				if parameter.In == "query" && kind == "array" {
					input["minItems"] = max(uint64(1), schema.MinItems)
				}
				if parameter.Description != "" {
					input["description"] = parameter.Description
				}
				group := groups[parameter.In]
				group["properties"].(map[string]any)[parameter.Name] = input
				if parameter.Required {
					group["required"] = append(group["required"].([]string), parameter.Name)
				}
				operation.Parameters = append(operation.Parameters, ref)
			}
			input := objectSchema()
			required := []string{}
			for _, groupName := range []string{"path", "query"} {
				group := groups[groupName]
				if len(group["properties"].(map[string]any)) > 0 {
					input["properties"].(map[string]any)[groupName] = group
					if len(group["required"].([]string)) > 0 {
						required = append(required, groupName)
					}
				}
			}
			if definition.RequestBody != nil {
				body := definition.RequestBody.Value
				if body == nil || len(body.Content) != 1 || body.Content["application/json"] == nil || body.Content["application/json"].Schema == nil || body.Content["application/json"].Schema.Value == nil {
					reason = "only application/json request bodies are supported"
				} else {
					operation.Body = body.Content["application/json"].Schema
					operation.BodyRequired = body.Required
					converted, conversionErr := inputSchema(operation.Body.Value, operation.JSONSchema2020, make(map[*openapi3.Schema]bool), 0, &budget)
					if conversionErr != nil {
						reason = conversionErr.Error()
					} else {
						input["properties"].(map[string]any)["body"] = converted
						if body.Required {
							required = append(required, "body")
						}
					}
				}
			}
			input["required"] = required
			description := definition.Description
			if description == "" {
				description = definition.Summary
			}
			if description == "" {
				description = method + " " + path
			}
			readOnly := method == http.MethodGet || method == http.MethodHead
			destructive := !readOnly
			operation.Tool = &mcp.Tool{Name: name, Description: description, InputSchema: input, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive, IdempotentHint: readOnly || method == http.MethodPut || method == http.MethodDelete}}
			encoded, _ := json.Marshal(operation.Tool)
			if len(encoded) > 64<<10 {
				reason = "generated tool schema exceeds 64 KiB"
			}
			if len(catalog.Operations) >= 128 || size+len(encoded) > MaxBytes {
				reason = "catalog size limit exceeded"
			}
			if reason != "" {
				catalog.Skipped = append(catalog.Skipped, SkippedOperation{Name: name, Reason: reason})
				continue
			}
			catalog.Operations = append(catalog.Operations, operation)
			size += len(encoded)
		}
	}
	return catalog, nil
}

func objectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false}
}

func inputKind(schema *openapi3.Schema) (string, error) {
	types := schema.Type.Slice()
	kind := ""
	for _, value := range types {
		if value == "null" {
			continue
		}
		if kind != "" {
			return "", errors.New("only nullable type unions are supported")
		}
		kind = value
	}
	if kind == "" && len(types) == 1 {
		kind = types[0]
	}
	if kind == "" && len(schema.Properties) > 0 {
		kind = "object"
	}
	if kind == "" && schema.Items != nil {
		kind = "array"
	}
	switch kind {
	case "", "string", "number", "integer", "boolean", "object", "array", "null":
		return kind, nil
	}
	return "", errors.New("unsupported input type")
}

func inputSchema(schema *openapi3.Schema, draft2020 bool, active map[*openapi3.Schema]bool, depth int, nodes *int) (map[string]any, error) {
	if active[schema] {
		return nil, errors.New("unsupported recursive MCP input schema")
	}
	*nodes++
	if depth > 12 || *nodes > 2048 {
		return nil, errors.New("input schema complexity limit exceeded")
	}
	if len(schema.OneOf)+len(schema.AnyOf)+len(schema.AllOf)+len(schema.PrefixItems)+len(schema.PatternProperties)+len(schema.DependentSchemas)+len(schema.DependentRequired) > 0 || schema.Not != nil || schema.If != nil || schema.Then != nil || schema.Else != nil || schema.Contains != nil || schema.PropertyNames != nil || schema.Discriminator != nil || schema.UnevaluatedItems.Has != nil || schema.UnevaluatedItems.Schema != nil || schema.UnevaluatedProperties.Has != nil || schema.UnevaluatedProperties.Schema != nil || schema.Always != nil || schema.Const != nil {
		return nil, errors.New("unsupported complex MCP input schema")
	}
	if schema.Format == "binary" || schema.Format == "byte" || schema.ContentEncoding != "" || schema.ContentMediaType != "" || schema.ContentSchema != nil {
		return nil, errors.New("binary input schemas are not supported")
	}
	if schema.SchemaDialect != "" && schema.SchemaDialect != "https://json-schema.org/draft/2020-12/schema" && schema.SchemaDialect != "https://spec.openapis.org/oas/3.1/dialect/base" {
		return nil, errors.New("unsupported JSON Schema dialect")
	}
	if draft2020 && schema.Nullable {
		return nil, errors.New("OpenAPI 3.1 nullable inputs must use a type union")
	}
	kind, err := inputKind(schema)
	if err != nil {
		return nil, err
	}
	active[schema] = true
	defer delete(active, schema)
	result := map[string]any{}
	if kind != "" {
		result["type"] = kind
	}
	if kind != "null" && (schema.Nullable || schema.Type.Includes("null")) {
		result["type"] = []string{kind, "null"}
	}
	if schema.Description != "" {
		result["description"] = schema.Description
	}
	if schema.Format != "" {
		result["format"] = schema.Format
	}
	if schema.Enum != nil {
		result["enum"] = schema.Enum
	}
	if schema.Min != nil {
		result["minimum"] = *schema.Min
	}
	if schema.Max != nil {
		result["maximum"] = *schema.Max
	}
	if schema.MultipleOf != nil {
		result["multipleOf"] = *schema.MultipleOf
	}
	if schema.ExclusiveMin.Value != nil {
		result["exclusiveMinimum"] = *schema.ExclusiveMin.Value
	} else if schema.ExclusiveMin.IsTrue() && schema.Min != nil {
		result["exclusiveMinimum"] = *schema.Min
		delete(result, "minimum")
	}
	if schema.ExclusiveMax.Value != nil {
		result["exclusiveMaximum"] = *schema.ExclusiveMax.Value
	} else if schema.ExclusiveMax.IsTrue() && schema.Max != nil {
		result["exclusiveMaximum"] = *schema.Max
		delete(result, "maximum")
	}
	if schema.MinLength > 0 {
		result["minLength"] = schema.MinLength
	}
	if schema.MaxLength != nil {
		result["maxLength"] = *schema.MaxLength
	}
	if schema.Pattern != "" {
		result["pattern"] = schema.Pattern
	}
	if schema.MinItems > 0 {
		result["minItems"] = schema.MinItems
	}
	if schema.MaxItems != nil {
		result["maxItems"] = *schema.MaxItems
	}
	if schema.UniqueItems {
		result["uniqueItems"] = true
	}
	if schema.MinProps > 0 {
		result["minProperties"] = schema.MinProps
	}
	if schema.MaxProps != nil {
		result["maxProperties"] = *schema.MaxProps
	}
	if kind == "array" {
		if schema.Items == nil || schema.Items.Value == nil {
			return nil, errors.New("array input requires an item schema")
		}
		items, err := inputSchema(schema.Items.Value, draft2020, active, depth+1, nodes)
		if err != nil {
			return nil, err
		}
		result["items"] = items
	}
	if kind == "object" {
		properties := map[string]any{}
		required := []string{}
		keys := []string{}
		for key := range schema.Properties {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			ref := schema.Properties[key]
			if ref.Value.ReadOnly {
				continue
			}
			property, err := inputSchema(ref.Value, draft2020, active, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			properties[key] = property
			if slices.Contains(schema.Required, key) {
				required = append(required, key)
			}
		}
		result["properties"] = properties
		result["required"] = required
		if schema.AdditionalProperties.Has != nil {
			result["additionalProperties"] = *schema.AdditionalProperties.Has
		} else if schema.AdditionalProperties.Schema != nil {
			additional, err := inputSchema(schema.AdditionalProperties.Schema.Value, draft2020, active, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			result["additionalProperties"] = additional
		}
	}
	return result, nil
}

func validBackend(address *url.URL) bool {
	return address != nil && address.Host != "" && (address.Scheme == "http" || address.Scheme == "https") && address.User == nil && address.RawQuery == "" && address.Fragment == "" && !strings.ContainsAny(address.Path, "{}") && !unsafeSegments(address.Path)
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func unsafeSegments(path string) bool {
	if strings.ContainsAny(path, "\\\x00\r\n") {
		return true
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}
