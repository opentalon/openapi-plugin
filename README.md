# openapi-plugin

An [OpenTalon](https://github.com/opentalon/opentalon) plugin that exposes a REST
API — described by an **OpenAPI 3** document — as workflow tools. It is the REST
counterpart of [`mcp-plugin`](https://github.com/opentalon/mcp-plugin): same host
contract, different transport.

## What it does

1. **Configure** — fetches the OpenAPI doc named in `spec_url`, resolves `$ref`s
   and `allOf` composition, and builds one action per operation. Names prefer
   `operationId`, else are synthesised deterministically from method + path
   (`GET /items` → `list_items`, `PATCH /items/{id}` → `update_item`,
   `POST /items/{id}/reserve` → `reserve_item`). A request body that is a single
   object property (`{"item": {...}}`) is auto-unwrapped — its inner fields
   become flat params and are re-nested at execution.
2. **Capabilities** — returns those operations as actions (with each parameter's
   JSON Schema passed through, `ReadOnly` for `GET`) so the orchestrator can sync
   them into RAG and offer them to the LLM. Per-route `x-llm-description` and
   `x-synonyms` extensions are appended to the action text the LLM sees.
3. **Execute** — turns a `tool "<server>" "<operation>" { args }` call into the
   HTTP request the operation describes: path params substituted, query params
   set, JSON body coerced to each property's schema type (an integer id
   serialises as `42`, a list arg as an array). A 2xx JSON body is returned as
   `StructuredContent` so later steps can navigate it (`step("s").field`).

## Identity / auth

Per-user identity is **not** configured here. On every call the host injects the
caller's **credential headers** (from WhoAmI — e.g. a user-id header), and the
plugin forwards them verbatim, so the request runs **as the workflow's owner**.
Static, non-identity headers (a service bearer) go in `headers`.

## Config (in OpenTalon `config.yaml`)

```yaml
plugins:
  # The map key is the plugin id — OpenTalon builds the binary as this name
  # (via `make build BINARY_NAME=<key>`), so it can be anything. Use
  # `server_name` for the tln server the LLM calls.
  inventory-api:
    github: "opentalon/openapi-plugin"
    ref: "master"
    config:
      server_name: "inventory-api"                # tln server name: tool "inventory-api" "…"
      spec_url:    "http://localhost:8080/openapi.json"
      base_url:    "http://localhost:8080"
      headers:                                     # static, ${ENV} expanded
        X-Service-Token: "${SERVICE_TOKEN}"
      allowlist: [create_item, update_item, list_categories, reserve_item]  # optional
      timeout: "15s"
      extra_operations:                            # ops NOT in the spec (service-only routes)
        - name: send_alert
          method: POST
          path: /api/v1/alerts
          summary: send an alert to a user
          params:
            - {name: to,   in: body, type: string, required: true}
            - {name: text, in: body, type: string, required: true}
        - name: create_order                       # flat args re-nested under "order" at execution
          method: POST
          path: /api/v1/orders
          body_wrap: order
          params:
            - {name: item_id,  in: body, type: integer, required: true}
            - {name: quantity, in: body, type: integer, required: true}
```

| Field | Required | Description |
|---|---|---|
| `server_name` | no (default `api`) | the tln server name: `tool "<server_name>" "…"` |
| `spec_url` | **yes** | OpenAPI 3 document URL (`${ENV}` expanded) |
| `base_url` | **yes** | API base for calls (`${ENV}` expanded) |
| `headers` | no | static request headers; values support `${ENV}` |
| `allowlist` | no | if set, only these **spec** operation names are exposed (extra_operations are always exposed) |
| `extra_operations` | no | hand-declared ops the spec omits (a service-only route); each has `name`/`method`/`path` + `params` (`in`: path/query/body), optional `body_wrap` to nest body params under one key. Executes and RAG-syncs like a spec op. |
| `timeout` | no (default `15s`) | Go duration for spec fetch + calls |

## Build

```sh
make build   # go mod tidy && go build -o openapi-plugin .
make test
```

Depends on `github.com/opentalon/opentalon` `pkg/plugin` and
`github.com/getkin/kin-openapi` (spec parsing).
