# OpenAPI MCP Server

A reusable OpenAPI-to-MCP server written in Go. Use it as a standalone HTTP or
stdio server, embed it in another Go application, or deploy the optional AWS
Lambda adapter.

This first iteration provides one tool, `get_api_information`, which returns an
OpenAPI document's title, version and description. OpenAPI operation conversion
and REST execution are planned for later iterations.

## Run

Requires Go 1.25 or later.

```sh
OPENAPI_URL=https://api.example.com/openapi.json make run
```

Connect an MCP client to `http://127.0.0.1:8080/mcp` using Streamable HTTP.
For stdio:

```sh
OPENAPI_URL=https://api.example.com/openapi.json go run ./cmd/openapi-mcp-server -transport stdio
```

The tool takes an empty object (`{}`) and returns structured JSON and text:

```json
{
  "title": "Example API",
  "version": "1.2.3",
  "description": "Read public items from an example catalog."
}
```

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `OPENAPI_URL` | Required | Absolute HTTP(S) URL of the OpenAPI JSON document. |
| `MCP_PATH` | `/mcp` | HTTP path serving MCP. |
| `HTTP_ADDR` | `127.0.0.1:8080` | Standalone HTTP listen address. |
| `AUTH_HEADERS` | `Authorization,Cookie,X-Api-Key` | Comma-separated credential headers forwarded to the schema endpoint. Set empty to forward none. |

Configuration is operator supplied; tool callers cannot change the schema URL.
Redirects are rejected. Routing, body framing and connection headers cannot be
configured as authentication headers.

## Schema freshness and caller identity

MCP initialization and protocol discovery fetch the current schema anonymously,
even when the initializing client supplies credentials. Tool listing fetches it
again with that request's credentials. Each `get_api_information` call also
fetches it with the calling request's credentials, or anonymously when absent.
Only configured authentication headers are forwarded. A schema authentication
failure returns an error; it never falls back to anonymous metadata.

One server instance and HTTP client can serve multiple parallel requests.
Credentials and schema results stay request scoped: no shared cookie jar,
session credential cache, or global authenticated schema. A changed schema is
visible on the next listing or tool call. Invalid JSON-RPC requests, unknown
tools and transport acknowledgements do not fetch schema data.

Schema requests inherit the caller's context, time out after 10 seconds and
accept at most 1 MiB. HTTP MCP request bodies are also limited to 1 MiB. Logs are
JSON on stderr; tool failures return a generic message without credentials or
upstream response bodies.

The server decodes and validates the Info Object using
`github.com/getkin/kin-openapi/openapi3`. It does not resolve or fetch path
references. `github.com/modelcontextprotocol/go-sdk/mcp` provides the MCP protocol
and transport support. HTTP uses stateless, buffered JSON responses; GET and
DELETE return 405 and notifications do not open a persistent SSE stream.

## Embed or extend

The `proxy` package has no AWS runtime dependency. `NewServer` returns the SDK
server, so consumers can add tools, attach middleware or connect another MCP
transport. `NewHTTPHandler` adapts the same reusable server to `net/http`.

```go
server, err := proxy.NewServer(proxy.Config{
    SchemaURL: "https://api.example.com/openapi.json",
    AuthHeaders: []string{"Authorization", "X-Tenant-Token"},
})
if err != nil {
    return err
}
handler := proxy.NewHTTPHandler(server)
```

HTTP requests use their own headers automatically. For non-HTTP integrations,
`proxy.WithCredentials(ctx, headers)` copies credentials into a caller's server
connection context; pass that context to `server.Connect`. Use a separate
connection context per caller. Plain stdio has no HTTP headers and runs
anonymously unless the embedding application supplies credentials this way.

## Build

```sh
make test lint build
```

`dist/openapi-mcp-server` is the standalone binary for the current platform.
`make build-local` builds only that binary.

`make build-lambda` builds `dist/bootstrap.zip`, containing a static Linux ARM64
executable for `provided.al2023`. The separate `cmd/openapi-mcp-lambda` command
uses the AWS HTTP API v2 adapter. Set the Lambda handler to `bootstrap` and
configure `OPENAPI_URL`; `MCP_PATH` can match the gateway route. Infrastructure,
IAM and publication are managed by the consuming deployment.

Tests use independent fixtures and loopback servers. They cover official SDK
calls, concurrent credential isolation, anonymous initialization, schema changes,
authentication errors, non-HTTP reuse, bounds, redirects and Lambda envelopes.
CI runs race tests only and produces no binary artifacts. Consumers build their
own binaries using the targets above.
