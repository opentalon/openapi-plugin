package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// Config is one API surface: where to fetch its OpenAPI doc, where to call it,
// and which static headers to add. Per-user identity is NOT configured here —
// it arrives on each call via the host-injected credential headers.
type Config struct {
	ServerName  string            `json:"server_name"`      // tln server name, e.g. "timly-api"
	SpecURL     string            `json:"spec_url"`         // OpenAPI 3 document URL (supports ${ENV})
	BaseURL     string            `json:"base_url"`         // API base for calls (supports ${ENV})
	Headers     map[string]string `json:"headers"`          // static request headers, e.g. a service bearer (values support ${ENV})
	Allowlist   []string          `json:"allowlist"`        // if set, only these SPEC operation names are exposed (extra_operations are always exposed)
	ExtraOps    []ExtraOp         `json:"extra_operations"` // hand-declared ops not in the spec (e.g. an undocumented endpoint)
	Timeout     string            `json:"timeout"`          // Go duration; default 15s
	Description string            `json:"description"`      // capability description surfaced to the LLM
}

// ExtraOp declares an operation that is NOT in the OpenAPI doc — an endpoint the
// spec deliberately omits (e.g. a service-only route). It carries the same shape
// as a parsed operation so it merges into the action set and executes
// identically. `in` on each param is "path", "query", or "body".
type ExtraOp struct {
	Name        string         `json:"name"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	ReadOnly    bool           `json:"read_only"`
	Params      []ExtraOpParam `json:"params"`
}

// ExtraOpParam is one input to an ExtraOp.
type ExtraOpParam struct {
	Name     string          `json:"name"`
	In       string          `json:"in"` // "path" | "query" | "body"
	Type     string          `json:"type"`
	Required bool            `json:"required"`
	Desc     string          `json:"description"`
	Schema   json.RawMessage `json:"schema"`
}

type handler struct {
	cfg     Config
	ops     map[string]operation
	headers map[string]string // env-expanded Config.Headers
	client  *http.Client
}

// Configure runs (Init RPC) before Capabilities, so the spec is loaded and the
// dynamic action set is ready by the time the host asks for capabilities.
func (h *handler) Configure(configJSON string) error {
	if strings.TrimSpace(configJSON) == "" {
		return fmt.Errorf("openapi-plugin: config is required")
	}
	var cfg Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("openapi-plugin: parse config: %w", err)
	}
	cfg.SpecURL = os.Expand(cfg.SpecURL, os.Getenv)
	cfg.BaseURL = os.Expand(cfg.BaseURL, os.Getenv)
	if cfg.SpecURL == "" || cfg.BaseURL == "" {
		return fmt.Errorf("openapi-plugin: spec_url and base_url are required")
	}
	if cfg.ServerName == "" {
		cfg.ServerName = "api"
	}
	timeout := 15 * time.Second
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return fmt.Errorf("openapi-plugin: invalid timeout %q: %w", cfg.Timeout, err)
		}
		timeout = d
	}

	h.headers = map[string]string{}
	for k, v := range cfg.Headers {
		h.headers[k] = os.Expand(v, os.Getenv)
	}

	ops, err := loadSpec(context.Background(), cfg.SpecURL, timeout)
	if err != nil {
		return fmt.Errorf("openapi-plugin: %w", err)
	}
	if len(cfg.Allowlist) > 0 {
		allow := map[string]bool{}
		for _, n := range cfg.Allowlist {
			allow[n] = true
		}
		for n := range ops {
			if !allow[n] {
				delete(ops, n)
			}
		}
	}
	// Hand-declared operations are added AFTER allowlist filtering — they are
	// always exposed (the allowlist only curates the spec-derived set). They
	// override a same-named spec op.
	for _, e := range cfg.ExtraOps {
		o, err := e.toOperation()
		if err != nil {
			return fmt.Errorf("openapi-plugin: extra_operations: %w", err)
		}
		ops[o.Name] = o
	}
	if len(ops) == 0 {
		return fmt.Errorf("openapi-plugin: no operations (check spec_url / allowlist / extra_operations)")
	}

	h.cfg = cfg
	h.ops = ops
	h.client = &http.Client{Timeout: timeout}
	return nil
}

func (h *handler) Capabilities() plugin.CapabilitiesMsg {
	actions := make([]plugin.ActionMsg, 0, len(h.ops))
	for _, name := range sortedOpNames(h.ops) {
		o := h.ops[name]
		params := make([]plugin.ParameterMsg, 0, len(o.PathParams)+len(o.QueryParams)+len(o.BodyProps))
		for _, p := range o.PathParams {
			params = append(params, toParam(p))
		}
		for _, p := range o.QueryParams {
			params = append(params, toParam(p))
		}
		for _, p := range o.BodyProps {
			params = append(params, toParam(p))
		}
		actions = append(actions, plugin.ActionMsg{
			Name:        name,
			Description: describe(o),
			Parameters:  params,
			ReadOnly:    o.ReadOnly,
		})
	}
	desc := h.cfg.Description
	if desc == "" {
		desc = fmt.Sprintf("REST API operations (%s) exposed as workflow tools.", h.cfg.ServerName)
	}
	return plugin.CapabilitiesMsg{
		Name:        h.cfg.ServerName,
		Description: desc,
		Actions:     actions,
	}
}

func describe(o operation) string {
	if o.Description == "" {
		return o.Summary
	}
	if o.Summary == "" {
		return o.Description
	}
	return o.Summary + "\n\n" + o.Description
}

func toParam(p apiParam) plugin.ParameterMsg {
	t := p.Type
	if t == "" {
		t = "string"
	}
	return plugin.ParameterMsg{
		Name:        p.Name,
		Description: p.Desc,
		Type:        t,
		Required:    p.Required,
		Schema:      p.Schema,
	}
}

func sortedOpNames(ops map[string]operation) []string {
	names := make([]string, 0, len(ops))
	for n := range ops {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// synthName derives a deterministic action name from method+path when the spec
// omits operationId: list_<plural> / show_<singular> / create_<singular> /
// update_<singular> / delete_<singular>, and <action>_<singular> for a
// sub-resource verb like /items/{id}/checkout → checkout_item.
func synthName(method, path string) string {
	var core []string
	hasID := false
	for _, s := range strings.Split(path, "/") {
		switch {
		case s == "":
		case strings.HasPrefix(s, "{"):
			hasID = true
		default:
			core = append(core, s)
		}
	}
	if len(core) == 0 {
		return method
	}
	// Sub-resource action: has an id but does not end in it (…/{id}/checkout).
	if hasID && !strings.HasSuffix(strings.TrimRight(path, "/"), "}") {
		action := core[len(core)-1]
		base := action
		if len(core) >= 2 {
			base = singular(core[len(core)-2])
		}
		return action + "_" + base
	}
	base := core[len(core)-1]
	switch method {
	case "get":
		if hasID {
			return "show_" + singular(base)
		}
		return "list_" + base
	case "post":
		return "create_" + singular(base)
	case "put", "patch":
		return "update_" + singular(base)
	case "delete":
		return "delete_" + singular(base)
	}
	return method + "_" + base
}

func singular(w string) string {
	switch {
	case strings.HasSuffix(w, "ies"):
		return strings.TrimSuffix(w, "ies") + "y"
	case strings.HasSuffix(w, "ses"):
		return strings.TrimSuffix(w, "es")
	case strings.HasSuffix(w, "s"):
		return strings.TrimSuffix(w, "s")
	}
	return w
}
