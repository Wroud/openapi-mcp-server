package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type challengeKey struct{}

type challengeHolder struct {
	lock      sync.Mutex
	challenge string
}

func (holder *challengeHolder) set(challenge string) {
	holder.lock.Lock()
	defer holder.lock.Unlock()
	holder.challenge = challenge
}

func (holder *challengeHolder) get() string {
	holder.lock.Lock()
	defer holder.lock.Unlock()
	return holder.challenge
}

type challenger struct {
	metadataURL string
	scope       string
	userAgents  []string
	public      mcp.Meta
	protected   mcp.Meta
}

type securityScheme struct {
	Type   string `json:"type"`
	Scopes any    `json:"scopes,omitempty"`
}

func newChallenger(config Config) (*challenger, error) {
	if config.ResourceMetadataURL == "" {
		return nil, nil
	}
	metadata, err := url.Parse(config.ResourceMetadataURL)
	if err != nil || metadata.Host == "" || (metadata.Scheme != "https" && metadata.Scheme != "http") || metadata.User != nil || !quotable(config.ResourceMetadataURL) {
		return nil, errors.New("invalid ResourceMetadataURL")
	}
	if !quotable(config.ChallengeScope) {
		return nil, errors.New("invalid ChallengeScope")
	}
	scopes := []string{}
	if config.ChallengeScope != "" {
		scopes = append(scopes, config.ChallengeScope)
	}
	result := &challenger{
		metadataURL: config.ResourceMetadataURL,
		scope:       config.ChallengeScope,
		public:      mcp.Meta{"securitySchemes": []securityScheme{{Type: "noauth"}}},
		protected:   mcp.Meta{"securitySchemes": []securityScheme{{Type: "oauth2", Scopes: scopes}}},
	}
	for _, agent := range config.ToolResultChallengeUserAgents {
		if agent = strings.ToLower(strings.TrimSpace(agent)); agent != "" {
			result.userAgents = append(result.userAgents, agent)
		}
	}
	return result, nil
}

func quotable(value string) bool {
	for _, char := range value {
		if char < 0x20 || char > 0x7e || char == '"' || char == '\\' {
			return false
		}
	}
	return true
}

func (c *challenger) header(description string) string {
	value := `Bearer resource_metadata="` + c.metadataURL + `"`
	if c.scope != "" {
		value += `, scope="` + c.scope + `"`
	}
	if description != "" {
		value += `, error="invalid_token", error_description="` + description + `"`
	}
	return value
}

func (c *challenger) schemes(protected bool) mcp.Meta {
	if protected {
		return c.protected
	}
	return c.public
}

func (c *challenger) respond(ctx context.Context, tool string, headers http.Header, rejected bool) *mcp.CallToolResult {
	description, reason := "Sign in to use this tool", "missing"
	if rejected {
		description, reason = "The access token is invalid or expired", "rejected"
	}
	result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: description}}}
	agent := strings.ToLower(headers.Get("User-Agent"))
	logChallenge := func(form string) {
		slog.InfoContext(ctx, "Sign-in challenge", "tool", tool, "reason", reason, "form", form, "user_agent", headers.Get("User-Agent"), "authorization_header", headers.Get("Authorization") != "")
	}
	for _, candidate := range c.userAgents {
		if strings.Contains(agent, candidate) {
			logChallenge("tool_result")
			result.Meta = mcp.Meta{"mcp/www_authenticate": []string{c.header(description)}}
			return result
		}
	}
	logChallenge("http")
	if holder, ok := ctx.Value(challengeKey{}).(*challengeHolder); ok {
		if rejected {
			holder.set(c.header(description))
		} else {
			holder.set(c.header(""))
		}
	}
	return result
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (response *bufferedResponse) Header() http.Header { return response.header }

func (response *bufferedResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}

func (response *bufferedResponse) Write(data []byte) (int, error) {
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response.body.Write(data)
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }

func singleMethod(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return ""
	}
	var message struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &message) != nil {
		return ""
	}
	return message.Method
}

func NewHTTPHandler(server *mcp.Server) http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBytes)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			transport.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), failingReader{err}))
			transport.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		method := singleMethod(body)
		if method != "tools/call" && method != "tools/list" {
			transport.ServeHTTP(w, r)
			return
		}
		holder := &challengeHolder{}
		buffered := &bufferedResponse{header: w.Header()}
		transport.ServeHTTP(buffered, r.WithContext(context.WithValue(r.Context(), challengeKey{}, holder)))
		if challenge := holder.get(); challenge != "" {
			w.Header().Del("Content-Type")
			w.Header().Del("Content-Length")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		output := buffered.body.Bytes()
		if method == "tools/list" {
			output = withTopLevelSecuritySchemes(output)
		}
		if w.Header().Get("Content-Length") != "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(output)))
		}
		if buffered.status == 0 {
			buffered.status = http.StatusOK
		}
		w.WriteHeader(buffered.status)
		w.Write(output)
	})
}

func withTopLevelSecuritySchemes(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"securitySchemes"`)) {
		return body
	}
	var envelope, result map[string]json.RawMessage
	var tools []map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || json.Unmarshal(envelope["result"], &result) != nil || json.Unmarshal(result["tools"], &tools) != nil {
		return body
	}
	changed := false
	for _, tool := range tools {
		var meta map[string]json.RawMessage
		if json.Unmarshal(tool["_meta"], &meta) == nil && meta["securitySchemes"] != nil {
			tool["securitySchemes"] = meta["securitySchemes"]
			changed = true
		}
	}
	if !changed {
		return body
	}
	encoded, err := compactJSON(tools)
	if err != nil {
		return body
	}
	result["tools"] = encoded
	if encoded, err = compactJSON(result); err != nil {
		return body
	}
	envelope["result"] = encoded
	if encoded, err = compactJSON(envelope); err != nil {
		return body
	}
	return encoded
}

func compactJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
