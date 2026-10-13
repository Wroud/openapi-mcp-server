package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const discoverBody = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{
"io.modelcontextprotocol/protocolVersion":"2026-07-28",
"io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"2.1.296"},
"io.modelcontextprotocol/clientCapabilities":{}}}}`

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.296"}}}`

func TestRejectsTheStreamingProtocol(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	request := httptest.NewRequest("POST", "/mcp", strings.NewReader(discoverBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "server/discover")
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	var envelope struct {
		Error struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	if envelope.Error.Code != -32022 {
		t.Fatalf("want unsupported protocol version, got HTTP %d: %s", response.Code, response.Body.String())
	}
	var data struct {
		Supported []string `json:"supported"`
	}
	if err := json.Unmarshal(envelope.Error.Data, &data); err != nil || len(data.Supported) == 0 || data.Supported[0] != "2025-11-25" {
		t.Fatalf("want supported legacy versions, got %s", envelope.Error.Data)
	}
}

func TestInitializeNegotiatesTheHandshakeProtocol(t *testing.T) {
	fixture := newProtectedFixture(t, challengeConfig())
	response := post(t, fixture.handler, initializeBody, http.Header{"Mcp-Protocol-Version": {"2026-07-28"}})
	if response.Code != 200 {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("negotiated %q", envelope.Result.ProtocolVersion)
	}
}
