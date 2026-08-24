package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// operation is one API endpoint the plugin exposes as a tln action.
type operation struct {
	Name        string // tool/action name (operationId, else synthesised)
	Method      string // GET, POST, PUT, PATCH, DELETE
	PathTmpl    string // e.g. /api/v1/tickets/{id}
	Summary     string
	Description string
	PathParams  []apiParam
	QueryParams []apiParam
	BodyProps   []apiParam
	ReadOnly    bool // GET → no confirmation gate
}

// apiParam is one input to an operation (path / query / body).
type apiParam struct {
	Name     string
	Type     string // JSON Schema primary type (integer/number/boolean/array/object/string)
	Required bool
	Desc     string
	Schema   json.RawMessage // the property's JSON Schema fragment, passed through to the LLM
}

// loadSpec fetches an OpenAPI 3 document and returns its operations keyed by
// action name. $refs are resolved by the loader. Names prefer operationId and
// otherwise are synthesised deterministically from method+path.
func loadSpec(ctx context.Context, specURL string, timeout time.Duration) (map[string]operation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, specURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch spec %s: %w", specURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch spec %s: HTTP %d", specURL, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read spec: %w", err)
	}

	loader := openapi3.NewLoader()
	loader.Context = ctx
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}

	ops := map[string]operation{}
	for path, item := range doc.Paths.Map() {
		if item == nil {
			continue
		}
		for method, op := range item.Operations() {
			o := buildOperation(method, path, op, item.Parameters)
			// Never collide two operations onto one name.
			name := o.Name
			for i := 2; ; i++ {
				if _, exists := ops[name]; !exists {
					break
				}
				name = fmt.Sprintf("%s_%d", o.Name, i)
			}
			o.Name = name
			ops[name] = o
		}
	}
	return ops, nil
}

func buildOperation(method, path string, op *openapi3.Operation, pathLevel openapi3.Parameters) operation {
	o := operation{
		Method:      strings.ToUpper(method),
		PathTmpl:    path,
		Summary:     op.Summary,
		Description: op.Description,
		ReadOnly:    strings.EqualFold(method, http.MethodGet),
	}
	if op.OperationID != "" {
		o.Name = op.OperationID
	} else {
		o.Name = synthName(strings.ToLower(method), path)
	}

	// Path-level parameters apply to every operation on the path; merge them in.
	params := append(openapi3.Parameters{}, pathLevel...)
	params = append(params, op.Parameters...)
	for _, ref := range params {
		if ref == nil || ref.Value == nil {
			continue
		}
		p := ref.Value
		ap := apiParam{Name: p.Name, Required: p.Required, Desc: p.Description}
		if p.Schema != nil {
			ap.Type = firstType(p.Schema)
			ap.Schema = marshalSchema(p.Schema)
		}
		switch p.In {
		case "path":
			ap.Required = true // path params are always required
			o.PathParams = append(o.PathParams, ap)
		case "query":
			o.QueryParams = append(o.QueryParams, ap)
		}
	}

	// Request body: the application/json object schema's properties.
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		if mt := op.RequestBody.Value.Content.Get("application/json"); mt != nil && mt.Schema != nil && mt.Schema.Value != nil {
			s := mt.Schema.Value
			required := map[string]bool{}
			for _, r := range s.Required {
				required[r] = true
			}
			names := make([]string, 0, len(s.Properties))
			for n := range s.Properties {
				names = append(names, n)
			}
			sort.Strings(names) // stable action schema across runs (hash-based sync skip)
			for _, n := range names {
				pr := s.Properties[n]
				ap := apiParam{Name: n, Required: required[n]}
				if pr != nil {
					ap.Type = firstType(pr)
					if pr.Value != nil {
						ap.Desc = pr.Value.Description
					}
					ap.Schema = marshalSchema(pr)
				}
				o.BodyProps = append(o.BodyProps, ap)
			}
		}
	}
	return o
}

func firstType(ref *openapi3.SchemaRef) string {
	if ref == nil || ref.Value == nil || ref.Value.Type == nil {
		return ""
	}
	if t := ref.Value.Type; t != nil && len(*t) > 0 {
		return (*t)[0]
	}
	return ""
}

// marshalSchema returns the resolved property schema as a JSON fragment for
// ParameterMsg.Schema. Returns nil (host synthesises from Type/Description) when
// it can't be announced safely.
func marshalSchema(ref *openapi3.SchemaRef) json.RawMessage {
	if ref == nil || ref.Value == nil {
		return nil
	}
	b, err := ref.Value.MarshalJSON()
	if err != nil || !json.Valid(b) {
		return nil
	}
	return json.RawMessage(b)
}
