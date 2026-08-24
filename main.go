// Command openapi-plugin is an OpenTalon plugin that exposes a REST API,
// described by an OpenAPI 3 document, as workflow tools. It fetches the spec
// named in its config, registers one action per operation (so the orchestrator
// can sync them into RAG and offer them to the LLM), and on execution turns a
// `tool "<server>" "<operation>" { ... }` call into the matching HTTP request —
// forwarding the caller's identity via the host-injected credential headers.
//
// It is the REST counterpart of mcp-plugin: same host contract, different
// transport. One config block == one API surface (server name).
package main

import (
	"os"

	"github.com/opentalon/opentalon/pkg/plugin"
)

func main() {
	if err := plugin.Serve(&handler{}); err != nil {
		os.Exit(1)
	}
}
