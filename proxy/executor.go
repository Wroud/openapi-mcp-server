package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Execute(ctx context.Context, client *http.Client, operation Operation, arguments json.RawMessage, credentials http.Header) *mcp.CallToolResult {
	failure := func(message string) *mcp.CallToolResult {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
	}
	if len(arguments) > MaxBytes {
		return failure("Arguments exceed the size limit")
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var input map[string]any
	argumentDecoder := json.NewDecoder(bytes.NewReader(arguments))
	argumentDecoder.UseNumber()
	if err := argumentDecoder.Decode(&input); err != nil || input == nil {
		return failure("Arguments must be a JSON object")
	}
	if err := argumentDecoder.Decode(new(any)); err != io.EOF {
		return failure("Arguments must contain one JSON object")
	}
	if err := checkJSONDepth(input, 0, 64); err != nil {
		return failure("Arguments exceed the nesting limit")
	}
	for key := range input {
		if key != "path" && key != "query" && key != "body" {
			return failure("Unknown argument field: " + key)
		}
	}
	groups := map[string]map[string]any{}
	for _, name := range []string{"path", "query"} {
		if value, present := input[name]; present {
			group, ok := value.(map[string]any)
			if !ok {
				return failure(name + " must be an object")
			}
			groups[name] = group
		} else {
			groups[name] = map[string]any{}
		}
	}
	path := operation.Path
	query := url.Values{}
	known := map[string]map[string]bool{"path": {}, "query": {}}
	options := []openapi3.SchemaValidationOption{openapi3.VisitAsRequest()}
	if operation.JSONSchema2020 {
		options = append(options, openapi3.EnableJSONSchema2020())
	}
	for _, ref := range operation.Parameters {
		parameter := ref.Value
		known[parameter.In][parameter.Name] = true
		value, present := groups[parameter.In][parameter.Name]
		if !present {
			if parameter.Required {
				return failure("Missing " + parameter.In + " parameter: " + parameter.Name)
			}
			continue
		}
		if err := parameter.Schema.Value.VisitJSON(value, options...); err != nil {
			return failure("Invalid " + parameter.In + " parameter: " + parameter.Name)
		}
		if parameter.In == "path" {
			scalar, err := parameterText(value, parameter.Schema.Value)
			if err != nil || unsafeSegments(scalar) {
				return failure("Unsafe or invalid path parameter: " + parameter.Name)
			}
			path = strings.ReplaceAll(path, "{"+parameter.Name+"}", url.PathEscape(scalar))
		} else if values, ok := value.([]any); ok {
			if len(values) == 0 {
				return failure("Query arrays must contain at least one value: " + parameter.Name)
			}
			for _, item := range values {
				text, err := parameterText(item, parameter.Schema.Value.Items.Value)
				if err != nil {
					return failure("Invalid query array: " + parameter.Name)
				}
				query.Add(parameter.Name, text)
			}
		} else {
			text, err := parameterText(value, parameter.Schema.Value)
			if err != nil {
				return failure("Invalid query parameter: " + parameter.Name)
			}
			query.Add(parameter.Name, text)
		}
	}
	for name, group := range groups {
		for key := range group {
			if !known[name][key] {
				return failure("Unknown " + name + " parameter: " + key)
			}
		}
	}
	if strings.ContainsAny(path, "{}") {
		return failure("Missing path parameter")
	}
	var body []byte
	value, present := input["body"]
	if operation.Body == nil && present {
		return failure("This operation has no request body")
	}
	if operation.BodyRequired && !present {
		return failure("Missing request body")
	}
	if operation.Body != nil && present {
		if err := operation.Body.Value.VisitJSON(value, options...); err != nil {
			return failure("Invalid request body")
		}
		var err error
		body, err = json.Marshal(value)
		if err != nil || len(body) > MaxBytes {
			return failure("Request body exceeds the size limit")
		}
	}
	if !validBackend(operation.Backend) || !strings.HasPrefix(path, "/") {
		return failure("Unsafe backend destination")
	}
	target := *operation.Backend
	escaped := strings.TrimSuffix(target.EscapedPath(), "/") + path
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return failure("Invalid request path")
	}
	target.Path = decoded
	target.RawPath = escaped
	target.RawQuery = query.Encode()
	if !sameOrigin(&target, operation.Backend) {
		return failure("Unsafe backend destination")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, operation.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return failure("Could not construct backend request")
	}
	request.Header = credentials.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Accept", "application/json, text/plain")
	if present && operation.Body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	transport := *http.DefaultClient
	if client != nil {
		transport = *client
	}
	transport.Jar = nil
	transport.Timeout = 10 * time.Second
	transport.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request)
	if err != nil {
		return failure("Backend request failed or timed out")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBytes+1))
	if err != nil {
		return failure("Could not read backend response")
	}
	if len(data) > MaxBytes {
		return failure(fmt.Sprintf("HTTP %d: response exceeds the size limit", response.StatusCode))
	}
	result := &mcp.CallToolResult{}
	unsuccessful := response.StatusCode < 200 || response.StatusCode >= 300
	var decodedJSON any
	responseDecoder := json.NewDecoder(bytes.NewReader(data))
	responseDecoder.UseNumber()
	parsed := responseDecoder.Decode(&decodedJSON) == nil && responseDecoder.Decode(new(any)) == io.EOF
	text := strings.ToValidUTF8(string(data), "�")
	if unsuccessful {
		if parsed {
			decodedJSON = redactJSON(decodedJSON, credentials)
			encoded, _ := json.Marshal(decodedJSON)
			text = string(encoded)
		} else {
			text = redactText(text, credentials)
		}
		text = strings.Map(func(value rune) rune {
			if unicode.IsControl(value) && value != '\n' && value != '\t' {
				return ' '
			}
			return value
		}, text)
		result.IsError = true
		result.StructuredContent = map[string]any{"status": response.StatusCode, "body": text}
		text = fmt.Sprintf("HTTP %d: %s", response.StatusCode, text)
	} else if parsed {
		encoded, _ := json.Marshal(decodedJSON)
		text = string(encoded)
		if object, ok := decodedJSON.(map[string]any); ok {
			result.StructuredContent = object
		}
	} else if len(data) == 0 {
		text = fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	result.Content = []mcp.Content{&mcp.TextContent{Text: text}}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 2*MaxBytes {
		return failure(fmt.Sprintf("HTTP %d: encoded response exceeds the size limit", response.StatusCode))
	}
	return result
}

func scalarText(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case json.Number:
		return value.String(), nil
	}
	return "", fmt.Errorf("expected a scalar")
}

func redactText(text string, headers http.Header) string {
	for _, values := range headers {
		for _, value := range values {
			if value != "" {
				text = strings.ReplaceAll(text, value, "[redacted]")
			}
			if strings.HasPrefix(value, "Bearer ") {
				token := strings.TrimPrefix(value, "Bearer ")
				if token != "" {
					text = strings.ReplaceAll(text, token, "[redacted]")
				}
			}
		}
	}
	request := &http.Request{Header: headers}
	for _, cookie := range request.Cookies() {
		if cookie.Value != "" {
			text = strings.ReplaceAll(text, cookie.Value, "[redacted]")
		}
	}
	return text
}

func redactJSON(value any, headers http.Header) any {
	switch node := value.(type) {
	case string:
		return redactText(node, headers)
	case []any:
		for i, item := range node {
			node[i] = redactJSON(item, headers)
		}
	case map[string]any:
		result := make(map[string]any, len(node))
		for key, item := range node {
			result[redactText(key, headers)] = redactJSON(item, headers)
		}
		return result
	}
	return value
}

func parameterText(value any, schema *openapi3.Schema) (string, error) {
	if number, ok := value.(json.Number); ok && schema.Type.Is("integer") {
		integer, ok := new(big.Rat).SetString(number.String())
		if !ok || !integer.IsInt() {
			return "", fmt.Errorf("invalid integer")
		}
		return integer.Num().String(), nil
	}
	return scalarText(value)
}
