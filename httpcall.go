package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// Execute turns a `tool "<server>" "<operation>" { args }` call into the HTTP
// request the OpenAPI operation describes, forwarding the caller's identity via
// the host-injected credential headers. A 2xx JSON body is returned as
// StructuredContent so later workflow steps can navigate it (step("s").field).
func (h *handler) Execute(req plugin.Request) plugin.Response {
	o, ok := h.ops[req.Action]
	if !ok {
		return plugin.Response{CallID: req.ID, Error: "openapi-plugin: unknown operation: " + req.Action}
	}

	httpReq, err := h.buildRequest(o, req.Args, req.CredentialHeaders)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: "openapi-plugin: " + err.Error()}
	}

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("openapi-plugin: %s %s: %v", o.Method, o.PathTmpl, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return plugin.Response{CallID: req.ID, Error: fmt.Sprintf("openapi-plugin: read response: %v", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plugin.Response{
			CallID: req.ID,
			Error:  fmt.Sprintf("openapi-plugin: %s %s -> HTTP %d: %s", o.Method, o.PathTmpl, resp.StatusCode, truncate(string(body), 600)),
		}
	}
	if len(body) > 0 && json.Valid(body) {
		return plugin.Response{CallID: req.ID, StructuredContent: string(body)}
	}
	return plugin.Response{CallID: req.ID, Content: string(body)}
}

func (h *handler) buildRequest(o operation, args map[string]string, creds map[string]plugin.CredentialHeader) (*http.Request, error) {
	// Path params — substitute {name}; all are required.
	path := o.PathTmpl
	for _, p := range o.PathParams {
		v, ok := args[p.Name]
		if !ok || v == "" {
			return nil, fmt.Errorf("%s: missing required path param %q", o.Name, p.Name)
		}
		path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(v))
	}

	u, err := url.Parse(strings.TrimRight(h.cfg.BaseURL, "/") + path)
	if err != nil {
		return nil, fmt.Errorf("%s: bad url: %w", o.Name, err)
	}

	// Query params.
	q := u.Query()
	for _, p := range o.QueryParams {
		if v, ok := args[p.Name]; ok && v != "" {
			q.Set(p.Name, v)
		}
	}
	u.RawQuery = q.Encode()

	// JSON body — coerce each supplied body prop to its schema type.
	var body io.Reader
	if len(o.BodyProps) > 0 {
		payload := map[string]any{}
		for _, p := range o.BodyProps {
			if v, ok := args[p.Name]; ok {
				payload[p.Name] = coerce(v, p.Type)
			}
		}
		if len(payload) > 0 {
			b, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("%s: marshal body: %w", o.Name, err)
			}
			body = bytes.NewReader(b)
		}
	}

	req, err := http.NewRequestWithContext(context.Background(), o.Method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Static configured headers (e.g. a service bearer).
	for k, v := range h.headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	// Forward whoami-provided credential headers (the caller's identity, e.g.
	// X-Timly-User-Id) — this is how the call runs AS the workflow's owner.
	for _, c := range creds {
		if c.Header != "" {
			req.Header.Set(c.Header, c.Value)
		}
	}
	return req, nil
}

// coerce converts a string arg (all tln args arrive as strings) to the JSON type
// the schema expects, so an integer id serialises as 42 not "42" and a list arg
// serialises as an array. Falls back to the raw string when parsing fails.
func coerce(raw, schemaType string) any {
	switch schemaType {
	case "integer":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	case "array", "object":
		var v any
		if json.Unmarshal([]byte(raw), &v) == nil {
			return v
		}
	}
	return raw
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
