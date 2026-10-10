package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

type SchemaLimits struct {
	DocumentBytes  int64
	TotalBytes     int64
	Documents      int
	ReferenceDepth int
	JSONDepth      int
	Timeout        time.Duration
}

type SchemaOptions struct {
	Client      *http.Client
	AllowedURLs []string
	Limits      SchemaLimits
}

type SchemaLoader struct {
	root    *url.URL
	client  *http.Client
	allowed map[string]bool
	limits  SchemaLimits
}

type schemaDocument struct {
	data     []byte
	tree     any
	location *url.URL
	full     bool
}

type schemaLoad struct {
	loader      *SchemaLoader
	ctx         context.Context
	credentials http.Header
	allowed     map[string]bool
	documents   map[string]*schemaDocument
	bytes       int64
	active      map[string]bool
	heights     map[string]int
}

func NewSchemaLoader(schemaURL string, options SchemaOptions) (*SchemaLoader, error) {
	root, err := url.Parse(schemaURL)
	if err != nil || root.Host == "" || (root.Scheme != "https" && root.Scheme != "http") || root.User != nil || root.Fragment != "" {
		return nil, errors.New("OPENAPI_URL must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	limits := options.Limits
	if limits.DocumentBytes == 0 {
		limits.DocumentBytes = MaxBytes
	}
	if limits.TotalBytes == 0 {
		limits.TotalBytes = 8 * MaxBytes
	}
	if limits.Documents == 0 {
		limits.Documents = 16
	}
	if limits.ReferenceDepth == 0 {
		limits.ReferenceDepth = 32
	}
	if limits.JSONDepth == 0 {
		limits.JSONDepth = 64
	}
	if limits.Timeout == 0 {
		limits.Timeout = 10 * time.Second
	}
	if limits.DocumentBytes < 1 || limits.TotalBytes < 1 || limits.Documents < 1 || limits.ReferenceDepth < 1 || limits.JSONDepth < 1 || limits.Timeout < 0 {
		return nil, errors.New("schema limits must be positive")
	}
	client := *http.DefaultClient
	if options.Client != nil {
		client = *options.Client
	}
	client.Timeout = limits.Timeout
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	loader := &SchemaLoader{root: root, client: &client, allowed: make(map[string]bool), limits: limits}
	for _, value := range options.AllowedURLs {
		location, err := url.Parse(value)
		if err != nil {
			return nil, errors.New("invalid admitted schema URL")
		}
		location = root.ResolveReference(location)
		if err := loader.checkLocation(location); err != nil {
			return nil, err
		}
		if location.Fragment != "" {
			return nil, errors.New("admitted schema URLs must not have fragments")
		}
		loader.allowed[documentKey(location)] = true
	}
	return loader, nil
}

func (loader *SchemaLoader) checkLocation(location *url.URL) error {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	if location.User != nil || location.Scheme != loader.root.Scheme || !strings.EqualFold(location.Hostname(), loader.root.Hostname()) || port(location) != port(loader.root) {
		return errors.New("schema reference must stay on the configured HTTP(S) origin")
	}
	return nil
}

func documentKey(location *url.URL) string {
	copy := *location
	copy.Fragment = ""
	copy.RawFragment = ""
	copy.Host = strings.ToLower(copy.Host)
	if copy.Scheme == "https" && copy.Port() == "443" || copy.Scheme == "http" && copy.Port() == "80" {
		copy.Host = copy.Hostname()
		if strings.Contains(copy.Host, ":") {
			copy.Host = "[" + copy.Host + "]"
		}
	}
	return copy.String()
}

func (loader *SchemaLoader) Load(ctx context.Context, credentials http.Header) (*openapi3.T, error) {
	ctx, cancel := context.WithTimeout(ctx, loader.limits.Timeout)
	defer cancel()
	state := &schemaLoad{loader: loader, ctx: ctx, credentials: credentials.Clone(), allowed: make(map[string]bool), documents: make(map[string]*schemaDocument), active: make(map[string]bool), heights: make(map[string]int)}
	for key := range loader.allowed {
		state.allowed[key] = true
	}
	state.allowed[documentKey(loader.root)] = true
	root, err := state.document(loader.root)
	if err != nil {
		return nil, err
	}
	if err := state.admit(root.tree, root.location, 0, false); err != nil {
		return nil, err
	}
	checked := make(map[string]bool)
	for {
		keys := make([]string, 0, len(state.documents))
		for key := range state.documents {
			if !checked[key] {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			break
		}
		slices.Sort(keys)
		for _, key := range keys {
			doc := state.documents[key]
			if _, err := state.walk(doc.tree, doc.location, 0, 0, false); err != nil {
				return nil, fmt.Errorf("schema %s: %w", doc.location.Path, err)
			}
			checked[key] = true
		}
	}
	var normalizedBytes int64
	for _, doc := range state.documents {
		data, err := json.Marshal(doc.tree)
		if err != nil {
			return nil, err
		}
		normalizedBytes += int64(len(data))
		if int64(len(data)) > loader.limits.DocumentBytes || normalizedBytes > loader.limits.TotalBytes {
			return nil, errors.New("normalized schema document or total byte limit exceeded")
		}
		doc.data = data
	}
	for _, doc := range state.documents {
		budget := &normalizationBudget{ctx: ctx, documentBytes: int64(len(doc.data)), totalBytes: &normalizedBytes, limits: loader.limits}
		if err := normalizeReferences(doc.tree, doc.location, false, budget); err != nil {
			return nil, err
		}
		data, err := json.Marshal(doc.tree)
		if err != nil {
			return nil, err
		}
		doc.data = data
	}
	parser := openapi3.NewLoader()
	parser.Context = ctx
	parser.IsExternalRefsAllowed = true
	parser.JoinFunc = func(base, ref *url.URL) *url.URL { return base.ResolveReference(ref) }
	parser.ReadFromURIFunc = func(_ *openapi3.Loader, location *url.URL) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := loader.checkLocation(location); err != nil {
			return nil, err
		}
		doc := state.documents[documentKey(location)]
		if doc == nil {
			return nil, errors.New("schema parser requested an unadmitted document")
		}
		return doc.data, nil
	}
	keys := make([]string, 0, len(state.documents))
	for key := range state.documents {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var entry *openapi3.T
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw := state.documents[key]
		if !raw.full {
			continue
		}
		doc, err := parser.LoadFromDataWithPath(raw.data, raw.location)
		if err != nil {
			return nil, fmt.Errorf("resolve schema %s: %w", raw.location.Path, err)
		}
		if err := doc.Validate(ctx); err != nil {
			return nil, fmt.Errorf("validate schema %s: %w", raw.location.Path, err)
		}
		if key == documentKey(loader.root) {
			entry = doc
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entry, nil
}

func (state *schemaLoad) document(location *url.URL) (*schemaDocument, error) {
	if err := state.ctx.Err(); err != nil {
		return nil, err
	}
	if err := state.loader.checkLocation(location); err != nil {
		return nil, err
	}
	key := documentKey(location)
	if doc := state.documents[key]; doc != nil {
		return doc, nil
	}
	if !state.allowed[key] {
		return nil, errors.New("schema URL is not admitted by the entry document or configuration")
	}
	if len(state.documents) >= state.loader.limits.Documents {
		return nil, errors.New("schema document count limit exceeded")
	}
	request, err := http.NewRequestWithContext(state.ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	request.Header = state.credentials.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Accept", "application/json")
	response, err := state.loader.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("schema %s returned HTTP %d", location.Path, response.StatusCode)
	}
	remaining := state.loader.limits.TotalBytes - state.bytes
	limit := min(remaining, state.loader.limits.DocumentBytes)
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("schema document or total byte limit exceeded")
	}
	var tree any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		return nil, fmt.Errorf("schema must be JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("schema has trailing JSON data")
	}
	if err := checkJSONDepth(tree, 0, state.loader.limits.JSONDepth); err != nil {
		return nil, err
	}
	object, objectOK := tree.(map[string]any)
	_, booleanOK := tree.(bool)
	if !objectOK && !booleanOK {
		return nil, errors.New("schema document must be a JSON object or boolean schema")
	}
	version, full := object["openapi"].(string)
	if _, present := object["openapi"]; present && !full {
		return nil, errors.New("invalid OpenAPI version")
	}
	if key == documentKey(state.loader.root) && !full {
		return nil, errors.New("entry must be an OpenAPI document")
	}
	if full {
		parts := strings.Split(version, ".")
		if len(parts) != 3 || parts[0] != "3" || (parts[1] != "0" && parts[1] != "1") {
			return nil, errors.New("only OpenAPI 3.0 and 3.1 documents are supported")
		}
		if _, err := strconv.ParseUint(parts[2], 10, 32); err != nil {
			return nil, errors.New("invalid OpenAPI version")
		}
	}
	canonical, err := url.Parse(key)
	if err != nil {
		return nil, err
	}
	doc := &schemaDocument{data: data, tree: tree, location: canonical, full: full}
	state.documents[key] = doc
	state.bytes += int64(len(data))
	return doc, nil
}

func skipSchemaData(key string, dictionary bool) bool {
	if dictionary {
		return false
	}
	return strings.HasPrefix(key, "x-") || key == "example" || key == "default" || key == "enum" || key == "const" || key == "value"
}

func (state *schemaLoad) admit(node any, base *url.URL, depth int, dictionary bool) error {
	if err := state.ctx.Err(); err != nil {
		return err
	}
	if depth > state.loader.limits.JSONDepth {
		return errors.New("schema JSON depth limit exceeded")
	}
	switch node := node.(type) {
	case map[string]any:
		if ref, ok := node["$ref"].(string); ok && !dictionary {
			parsed, err := url.Parse(ref)
			if err != nil {
				return errors.New("invalid schema reference URI")
			}
			location := base.ResolveReference(parsed)
			if err := state.loader.checkLocation(location); err != nil {
				return err
			}
			state.allowed[documentKey(location)] = true
		}
		for key, value := range node {
			if !skipSchemaData(key, dictionary) && !schemaExampleArray(key, value, dictionary) {
				if err := state.admit(value, base, depth+1, !dictionary && schemaDictionary(key)); err != nil {
					return err
				}
			}
		}
	case []any:
		for _, value := range node {
			if err := state.admit(value, base, depth+1, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (state *schemaLoad) walk(node any, base *url.URL, nesting, depth int, dictionary bool) (int, error) {
	if err := state.ctx.Err(); err != nil {
		return 0, err
	}
	if nesting > state.loader.limits.JSONDepth {
		return 0, errors.New("schema JSON depth limit exceeded")
	}
	height := 0
	switch node := node.(type) {
	case map[string]any:
		for _, keyword := range []string{"$anchor", "$dynamicRef", "$dynamicAnchor"} {
			if _, ok := node[keyword].(string); ok && !dictionary {
				return 0, fmt.Errorf("%s reference semantics are not supported", keyword)
			}
		}
		if id, ok := node["$id"].(string); ok && !dictionary {
			parsed, err := url.Parse(id)
			if err != nil {
				return 0, errors.New("invalid schema $id")
			}
			resolved := base.ResolveReference(parsed)
			if documentKey(resolved) != documentKey(base) || resolved.Fragment != "" {
				return 0, errors.New("schema $id that changes the reference base is not supported")
			}
		}
		if ref, ok := node["$ref"].(string); ok && !dictionary {
			parsed, err := url.Parse(ref)
			if err != nil {
				return 0, errors.New("invalid schema reference URI")
			}
			location := base.ResolveReference(parsed)
			if err := state.loader.checkLocation(location); err != nil {
				return 0, err
			}
			key := documentKey(location) + "#" + location.Fragment
			if !state.active[key] {
				next, done := state.heights[key]
				if !done {
					if depth >= state.loader.limits.ReferenceDepth {
						return 0, errors.New("schema reference depth limit exceeded")
					}
					doc, err := state.document(location)
					if err != nil {
						return 0, err
					}
					target, err := resolvePointer(doc.tree, location.Fragment)
					if err != nil {
						return 0, fmt.Errorf("reference %s: %w", location.Fragment, err)
					}
					state.active[key] = true
					next, err = state.walk(target, doc.location, 0, depth+1, false)
					delete(state.active, key)
					if err != nil {
						return 0, err
					}
					next++
					state.heights[key] = next
				}
				if depth+next > state.loader.limits.ReferenceDepth {
					return 0, errors.New("schema reference depth limit exceeded")
				}
				height = max(height, next)
			}
		}
		for key, value := range node {
			if skipSchemaData(key, dictionary) || schemaExampleArray(key, value, dictionary) {
				continue
			}
			next, err := state.walk(value, base, nesting+1, depth, !dictionary && schemaDictionary(key))
			if err != nil {
				return 0, err
			}
			height = max(height, next)
		}
	case []any:
		for _, value := range node {
			next, err := state.walk(value, base, nesting+1, depth, false)
			if err != nil {
				return 0, err
			}
			height = max(height, next)
		}
	}
	return height, nil
}

func resolvePointer(tree any, fragment string) (any, error) {
	if fragment == "" {
		return tree, nil
	}
	if !strings.HasPrefix(fragment, "/") {
		return nil, errors.New("only JSON Pointer reference fragments are supported")
	}
	for _, token := range strings.Split(fragment[1:], "/") {
		var decoded strings.Builder
		for i := 0; i < len(token); i++ {
			if token[i] != '~' {
				decoded.WriteByte(token[i])
				continue
			}
			i++
			if i == len(token) || (token[i] != '0' && token[i] != '1') {
				return nil, errors.New("invalid JSON Pointer escape")
			}
			if token[i] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
		}
		token = decoded.String()
		switch node := tree.(type) {
		case map[string]any:
			value, ok := node[token]
			if !ok {
				return nil, errors.New("JSON Pointer target does not exist")
			}
			tree = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) || len(token) > 1 && token[0] == '0' {
				return nil, errors.New("invalid JSON Pointer array index")
			}
			tree = node[index]
		default:
			return nil, errors.New("JSON Pointer crosses a scalar")
		}
	}
	return tree, nil
}

func loadInfo(ctx context.Context, client *http.Client, schemaURL string, credentials http.Header) (APIInfo, error) {
	loader, err := NewSchemaLoader(schemaURL, SchemaOptions{Client: client})
	if err != nil {
		return APIInfo{}, err
	}
	document, err := loader.Load(ctx, credentials)
	if err != nil {
		return APIInfo{}, err
	}
	return APIInfo{Title: document.Info.Title, Version: document.Info.Version, Description: document.Info.Description}, nil
}

func schemaDictionary(key string) bool {
	switch key {
	case "properties", "patternProperties", "$defs", "definitions", "schemas", "pathItems", "paths", "webhooks", "responses", "headers", "content", "callbacks", "requestBodies", "parameters", "examples", "securitySchemes", "links":
		return true
	}
	return false
}

func checkJSONDepth(node any, depth, limit int) error {
	if depth > limit {
		return errors.New("schema JSON depth limit exceeded")
	}
	switch node := node.(type) {
	case map[string]any:
		for _, value := range node {
			if err := checkJSONDepth(value, depth+1, limit); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range node {
			if err := checkJSONDepth(value, depth+1, limit); err != nil {
				return err
			}
		}
	}
	return nil
}

type normalizationBudget struct {
	ctx           context.Context
	documentBytes int64
	totalBytes    *int64
	limits        SchemaLimits
}

func normalizeReferences(node any, base *url.URL, dictionary bool, budget *normalizationBudget) error {
	if err := budget.ctx.Err(); err != nil {
		return err
	}
	switch node := node.(type) {
	case map[string]any:
		if ref, ok := node["$ref"].(string); ok && !dictionary {
			parsed, err := url.Parse(ref)
			if err != nil {
				return err
			}
			location := base.ResolveReference(parsed)
			canonical, err := url.Parse(documentKey(location))
			if err != nil {
				return err
			}
			canonical.Fragment = location.Fragment
			normalized := canonical.String()
			originalJSON, _ := json.Marshal(ref)
			normalizedJSON, _ := json.Marshal(normalized)
			growth := int64(len(normalizedJSON) - len(originalJSON))
			if budget.documentBytes+growth > budget.limits.DocumentBytes || *budget.totalBytes+growth > budget.limits.TotalBytes {
				return errors.New("normalized schema document or total byte limit exceeded")
			}
			budget.documentBytes += growth
			*budget.totalBytes += growth
			node["$ref"] = normalized
		}
		for key, value := range node {
			if !skipSchemaData(key, dictionary) && !schemaExampleArray(key, value, dictionary) {
				if err := normalizeReferences(value, base, !dictionary && schemaDictionary(key), budget); err != nil {
					return err
				}
			}
		}
	case []any:
		for _, value := range node {
			if err := normalizeReferences(value, base, false, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaExampleArray(key string, value any, dictionary bool) bool {
	_, array := value.([]any)
	return !dictionary && key == "examples" && array
}
