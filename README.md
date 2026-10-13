# OpenAPI MCP Server

A reusable OpenAPI-to-MCP server written in Go. Use it as a standalone HTTP or
stdio server, embed it in another Go application, or deploy the optional AWS
Lambda adapter.

The server exposes `get_api_information` and tools generated from supported
OpenAPI operations. It deliberately supports a practical subset rather than
interpreting every valid OpenAPI feature.

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

## Local development

Copy `.env.example` to `.env.local` and set `OPENAPI_URL` to the production schema
of the API you want to test. `.env.local` is ignored by Git and never shipped.

```sh
cp .env.example .env.local
make dev
```

The proxy runs locally at `http://127.0.0.1:8080/mcp`, fetching live schema metadata
from the configured upstream. In another terminal:

```sh
make smoke
make schema-check
```

`make schema-check` validates the complete live graph and prints only the entry
document's paths and operation IDs. The smoke client uses the official MCP SDK to initialize, verify tool discovery,
call `get_api_information` and print the returned metadata. It performs no
upstream API operations beyond fetching the schema. It is an explicit development
check, separate from the offline fixture tests in CI.

`HTTP_ADDR` and `MCP_PATH` control the local address and route. Set `DEV_ENV_FILE`
to use a different environment file. Stop the development server with Ctrl+C.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `OPENAPI_URL` | Required | Absolute HTTP(S) URL of the OpenAPI JSON document. |
| `BACKEND_URL` | First root server, or schema origin | Explicit HTTP(S) backend base URL override. |
| `MCP_PATH` | `/mcp` | HTTP path serving MCP. |
| `HTTP_ADDR` | `127.0.0.1:8080` | Standalone HTTP listen address. |
| `AUTH_HEADERS` | `Authorization,Cookie,X-Api-Key` | Comma-separated credential headers forwarded to the schema endpoint and the backend. Set empty to forward none. |
| `ANONYMOUS_SCHEMA` | `false` | `true` loads every schema document without caller credentials; credentials still go to the backend. |
| `RESOURCE_METADATA_URL` | Unset | OAuth protected-resource metadata URL. Enables sign-in challenges for protected operations; unset means no challenges. |
| `CHALLENGE_SCOPE` | Unset | Scope named in challenges and in protected tools' `securitySchemes`. |
| `TOOL_RESULT_CHALLENGE_USER_AGENTS` | Unset | Comma-separated, case-insensitive `User-Agent` substrings of clients that receive challenges as tool results instead of HTTP 401. |
| `SCHEMA_CACHE_TTL` | Unset | With `ANONYMOUS_SCHEMA=true`, a Go duration (`5m`) for which each instance reuses the last complete schema load. A degraded load is never cached; while a cached copy exists it is served instead. |
| `SCHEMA_TIMEOUT` | `10s` | Total deadline for one schema load. |
| `SCHEMA_SOURCE_TIMEOUT` | Half of `SCHEMA_TIMEOUT` | Deadline for each non-entry document. The entry's directly referenced documents are fetched in parallel. |

Configuration is operator supplied; tool callers cannot change the schema URL.
Redirects are rejected. Routing, body framing and connection headers cannot be
configured as authentication headers.

## Schema freshness and caller identity

With `Config.AnonymousSchema` (`ANONYMOUS_SCHEMA=true`) every schema load is
anonymous, so all callers see the same catalog; the paragraph below describes
the default credentialed loads. Configured credentials are still forwarded to
generated operations.

MCP initialization and protocol discovery fetch the current schema anonymously,
even when the initializing client supplies credentials. Tool listing fetches it
again with that request's credentials and generates only that caller's eligible
operations. Generated calls reload and resolve the operation against the current
schema before validating and executing it. Each `get_api_information` call also
fetches it with the calling request's credentials, or anonymously when absent.
Only configured authentication headers are forwarded. A schema authentication
failure returns an error; it never falls back to anonymous metadata.

One server instance and HTTP client can serve multiple parallel requests.
Credentials and schema results stay request scoped: no shared cookie jar,
session credential cache, or global authenticated schema. A changed schema is
visible on the next listing or tool call. Unknown operation names can refresh the schema, but never reach the REST backend.
Transport acknowledgements do not fetch schema data. Tools use stable operation
IDs; clients may need to refresh discovery after an API change. Listings are
private and immediately stale; no caller-specific tools enter the shared SDK registry.

Schema requests inherit the caller's context; the complete load has a 10-second
deadline. Each document is limited to 1 MiB, and the graph to 8 MiB and 16
documents. The byte limits also apply after URI normalization, preventing short
references from expanding into an oversized graph. JSON nesting is limited to 64
and reference depth to 32. HTTP MCP request bodies are also limited to 1 MiB. Logs are
JSON on stderr; tool failures return a generic message without credentials or
upstream response bodies.

`proxy.NewSchemaLoader` returns a concurrency-safe loader whose `Load` method
fetches, resolves and validates the entry and all referenced OpenAPI documents
using `github.com/getkin/kin-openapi/openapi3`. A loader reuses its HTTP client;
each load owns its graph, credentials and parser. A unique document is fetched
once per load. When a document reached from an entry Path Item `$ref` (directly
or through that source's own references) fails to fetch, parse or validate, the
entry Path Items that depend on it are dropped, the failure is logged with
`metric_name=UnhandledHttpError`, and the rest of the graph is returned. A
failure of the entry document, or of a source the entry also uses outside its
Path Items, still fails the load. Non-entry fetches are bounded by
`SchemaLimits.SourceTimeout` (default half of `Timeout`) so a slow source leaves
time for the rest. Later loads can recover without reusing stale authenticated
data.

External references must stay on the configured origin. Documents referenced
directly by the entry are admitted automatically; additional transitive document
URLs require `SchemaOptions.AllowedURLs`. Redirects and local-file references are
rejected. References are normalized against each source document before parsing;
URI fragments are decoded before JSON Pointer escapes. Only entry-selected paths
appear in the returned entry, even when a source contains other operations.

The compatibility profile covers JSON OpenAPI 3.0/3.1 documents, referenced
standalone JSON schemas, recursive local/external references, 3.0 `nullable`,
3.1 type unions and JSON Pointer fragments. `$id` is accepted when it leaves the
document base unchanged. Base-changing `$id`, named anchors and dynamic references
are rejected with diagnostics rather than silently misresolved. YAML, Swagger 2
and parser-unsupported OpenAPI features (including `components.pathItems`) are
not currently admitted. `SchemaOptions.Limits` can override the default bounds.

`github.com/modelcontextprotocol/go-sdk/mcp` provides the MCP protocol
and transport support. HTTP uses stateless, buffered JSON responses; GET and
DELETE return 405 and notifications do not open a persistent SSE stream.

## Supported REST operations

Only entry-selected paths are considered. Tool names use `operationId`, or a
stable method/path fallback; naming collisions fail discovery. Unsupported
operations are skipped with reasons shown by `make schema-check`.

Supported inputs are scalar path/query parameters, nonempty scalar query arrays
serialized as repeated keys, and `application/json` request bodies. Path Item
parameters are combined with operation parameters; operation definitions win.
MCP inputs use these groups:

```json
{
  "path": {"id": "123"},
  "query": {"tag": ["one", "two"]},
  "body": {"name": "Example"}
}
```

There is no headers argument. Configured credentials come from trusted request
context and are forwarded per call. Unknown groups/parameters, invalid values and
unsafe paths are rejected before any REST request.

Input schemas describe primitives, objects, arrays, required fields, enums,
bounds, patterns and nullable types. Runtime validation uses the original
kin-openapi schemas, enabling JSON Schema 2020-12 for OpenAPI 3.1. Polymorphic
compositions, recursive inputs, complex query objects, multipart, binary and
non-JSON request bodies are skipped. Conversion is bounded to 12 levels, 2,048
nodes and 64 KiB per tool; catalogs are limited to 128 tools and 1 MiB.

The first root server is used, resolving relative URLs against `OPENAPI_URL` and
substituting declared server-variable defaults. Without `BACKEND_URL`, its origin
must match the schema origin. An explicit backend override is trusted operator
configuration and replaces the server base URL. Path Item/operation server
overrides are skipped unless an explicit backend makes the destination clear.
Base paths are preserved and path arguments are encoded as segments.

REST requests have a 10-second deadline, inherit cancellation, reject redirects
and use no cookie jar or proxy-level retries. Requests and upstream responses are
limited to 1 MiB; encoded MCP results are capped at 2 MiB. JSON objects return
structured content plus text; arrays/scalars and non-JSON responses return text.
HTTP failures preserve their status with a bounded error body, redact forwarded
credentials and set `IsError`. Transport errors return concise proxy messages.
Responses are not validated against response schemas or streamed.

## Protected operations and sign-in challenges

An operation is protected when its effective `security` (operation level, else
root level) is non-empty and has no empty `{}` alternative. With
`Config.ResourceMetadataURL` set:

- Generated tools carry `securitySchemes`, both at the top level and in `_meta`:
  `[{"type":"noauth"}]` for public tools and `get_api_information`,
  `[{"type":"oauth2","scopes":[...]}]` for protected ones.
- Calling a protected tool without `Authorization` never reaches the backend
  and answers a challenge. A backend HTTP 401 to a protected call that sent
  `Authorization` answers a challenge with `error="invalid_token"`. Every other
  status, and any 401 from a public operation, stays an ordinary tool error.
- The challenge is `Bearer resource_metadata="<url>", scope="<scope>"`. Clients
  whose `User-Agent` matches `ToolResultChallengeUserAgents` receive an
  `isError` tool result with `_meta["mcp/www_authenticate"]` (always carrying
  `error` and `error_description`); others receive HTTP 401 with
  `WWW-Authenticate` and `Cache-Control: no-store`.

`NewHTTPHandler` buffers single-object `tools/call` and `tools/list` POSTs to
apply these rewrites; JSON-RPC batches pass through unbuffered and keep ordinary
tool errors. `initialize` logs the client's `User-Agent` and `clientInfo`,
never credentials.

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
configure `OPENAPI_URL`; `MCP_PATH` can match the gateway route. Set `BACKEND_URL` only when the root server is insufficient. Infrastructure,
IAM and publication are managed by the consuming deployment.

Tests use independent fixtures and loopback servers. They cover official SDK
calls, concurrent credential isolation, anonymous initialization and schema loads,
sign-in challenges in both forms, `securitySchemes` listings, schema changes,
authentication errors, non-HTTP reuse, bounds, redirects and Lambda envelopes. Graph fixtures also verify the seven
entry operations, deduplicated source fetches, source-local bases, escaped pointers,
recursion, null/union preservation, admission, per-source degradation and
failure recovery.
CI runs race tests only and produces no binary artifacts. Consumers build their
own binaries using the targets above.
