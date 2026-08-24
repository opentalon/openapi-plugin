# openapi-plugin

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that exposes a REST
API — described by an **OpenAPI 3** document — as workflow tools. It is the REST
counterpart of [`mcp-plugin`](https://github.com/opentalon/mcp-plugin): same host
contract, different transport.

## What it does

1. **Configure** — fetches the OpenAPI doc named in `spec_url`, resolves `$ref`s,
   and builds one action per operation. Names prefer `operationId`, else are
   synthesised deterministically from method + path (`GET /tickets` →
   `list_tickets`, `PATCH /tickets/{id}` → `update_ticket`,
   `POST /items/{id}/checkout` → `checkout_item`).
2. **Capabilities** — returns those operations as actions (with each parameter's
   JSON Schema passed through, `ReadOnly` for `GET`) so the orchestrator can sync
   them into RAG and offer them to the LLM.
3. **Execute** — turns a `tool "<server>" "<operation>" { args }` call into the
   HTTP request the operation describes: path params substituted, query params
   set, JSON body coerced to each property's schema type (an integer id
   serialises as `42`, a list arg as an array). A 2xx JSON body is returned as
   `StructuredContent` so later steps can navigate it (`step("s").field`).

## Identity / auth

Per-user identity is **not** configured here. On every call the host injects the
caller's **credential headers** (from WhoAmI — e.g. `X-Timly-User-Id`), and the
plugin forwards them verbatim, so the request runs **as the workflow's owner**.
Static, non-identity headers (a service bearer) go in `headers`.

## Config (in OpenTalon `config.yaml`)

```yaml
plugins:
  timly-api:
    plugin: "../openapi-plugin/openapi-plugin"
    config:
      server_name: "timly-api"                                  # tln server name
      spec_url:    "http://localhost:3000/api-docs/v1/swagger.yaml"
      base_url:    "http://localhost:3000"
      headers:                                                   # static, ${ENV} expanded
        X-Opentalon-MCP-Token: "${OPENTALON_MCP_TOKEN}"
      allowlist: [create_ticket, update_ticket, list_ticket_templates, checkout_item]  # optional
      timeout: "15s"
```

| Field | Required | Description |
|---|---|---|
| `server_name` | no (default `api`) | the tln server name: `tool "timly-api" "…"` |
| `spec_url` | **yes** | OpenAPI 3 document URL (`${ENV}` expanded) |
| `base_url` | **yes** | API base for calls (`${ENV}` expanded) |
| `headers` | no | static request headers; values support `${ENV}` |
| `allowlist` | no | if set, only these operation names are exposed |
| `timeout` | no (default `15s`) | Go duration for spec fetch + calls |

## Build

```sh
make build   # go mod tidy && go build -o openapi-plugin .
make test
```

Depends on `github.com/opentalon/opentalon` `pkg/plugin` and
`github.com/getkin/kin-openapi` (spec parsing).
