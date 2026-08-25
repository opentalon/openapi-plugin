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
	ReadOnly    bool   // GET → no confirmation gate
	BodyWrap    string // if set, nest body params under this key (Timly's {"ticket": {...}})
	LLMText     string // x-llm-description: extra guidance surfaced only to the LLM
	Synonyms    string // x-synonyms: alt phrasings, appended so RAG retrieval matches them
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
		LLMText:     extString(op.Extensions, "x-llm-description"),
		Synonyms:    extString(op.Extensions, "x-synonyms"),
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

	// Request body: the application/json object's properties, with allOf
	// composition resolved. A body that is a single object property (Timly's
	// {"ticket": {...}} wrapper) is auto-unwrapped — its inner props become the
	// flat params and o.BodyWrap re-nests them at execution.
	if op.RequestBody != nil && op.RequestBody.Value != nil {
		if mt := op.RequestBody.Value.Content.Get("application/json"); mt != nil && mt.Schema != nil && mt.Schema.Value != nil {
			props, required := collectProps(mt.Schema.Value, map[*openapi3.Schema]bool{})
			if wrapKey, inner := unwrapSingleObject(props); wrapKey != "" {
				o.BodyWrap = wrapKey
				props, required = collectProps(inner, map[*openapi3.Schema]bool{})
			}
			o.BodyProps = bodyParams(props, required)
		}
	}
	return o
}

// collectProps walks a schema's own properties plus those it composes via allOf
// (recursively, $refs resolved by the loader). oneOf/anyOf branch properties are
// also collected for the arg list, but their `required` is NOT — it is
// conditional per branch. Returns the merged property set and the unconditional
// required names.
func collectProps(s *openapi3.Schema, seen map[*openapi3.Schema]bool) (map[string]*openapi3.SchemaRef, []string) {
	props := map[string]*openapi3.SchemaRef{}
	var required []string
	if s == nil || seen[s] {
		return props, required
	}
	seen[s] = true

	for name, p := range s.Properties {
		props[name] = p
	}
	required = append(required, s.Required...)

	for _, sub := range s.AllOf {
		if sub != nil && sub.Value != nil {
			p, r := collectProps(sub.Value, seen)
			for k, v := range p {
				props[k] = v
			}
			required = append(required, r...)
		}
	}
	for _, group := range [][]*openapi3.SchemaRef{s.OneOf, s.AnyOf} {
		for _, sub := range group {
			if sub != nil && sub.Value != nil {
				p, _ := collectProps(sub.Value, seen)
				for k, v := range p {
					if _, ok := props[k]; !ok {
						props[k] = v
					}
				}
			}
		}
	}
	return props, dedupe(required)
}

// unwrapSingleObject returns (key, innerSchema) when props is exactly one entry
// whose value is an object (has its own props / allOf) — the wrapper convention.
// Otherwise ("", nil): a flat body.
func unwrapSingleObject(props map[string]*openapi3.SchemaRef) (string, *openapi3.Schema) {
	if len(props) != 1 {
		return "", nil
	}
	for key, ref := range props {
		if ref == nil || ref.Value == nil {
			return "", nil
		}
		v := ref.Value
		if len(v.Properties) > 0 || len(v.AllOf) > 0 || (v.Type != nil && v.Type.Is("object")) {
			return key, v
		}
	}
	return "", nil
}

// bodyParams turns a resolved property set into sorted apiParams (stable output
// for hash-based RAG sync).
func bodyParams(props map[string]*openapi3.SchemaRef, required []string) []apiParam {
	req := map[string]bool{}
	for _, r := range required {
		req[r] = true
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]apiParam, 0, len(names))
	for _, n := range names {
		pr := props[n]
		ap := apiParam{Name: n, Required: req[n]}
		if pr != nil {
			ap.Type = firstType(pr)
			if pr.Value != nil {
				ap.Desc = pr.Value.Description
			}
			ap.Schema = marshalSchema(pr)
		}
		out = append(out, ap)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// toOperation converts a hand-declared ExtraOp into the same operation shape a
// parsed spec op produces, so it merges into the action set and executes
// identically. Validates the essentials the config author can get wrong.
func (e ExtraOp) toOperation() (operation, error) {
	if e.Name == "" || e.Method == "" || e.Path == "" {
		return operation{}, fmt.Errorf("op %q: name, method and path are required", e.Name)
	}
	o := operation{
		Name:        e.Name,
		Method:      strings.ToUpper(e.Method),
		PathTmpl:    e.Path,
		Summary:     e.Summary,
		Description: e.Description,
		ReadOnly:    e.ReadOnly,
		BodyWrap:    e.BodyWrap,
	}
	for _, p := range e.Params {
		ap := apiParam{Name: p.Name, Type: p.Type, Required: p.Required, Desc: p.Desc, Schema: p.Schema}
		switch p.In {
		case "path":
			ap.Required = true
			o.PathParams = append(o.PathParams, ap)
		case "query":
			o.QueryParams = append(o.QueryParams, ap)
		case "body", "":
			o.BodyProps = append(o.BodyProps, ap)
		default:
			return operation{}, fmt.Errorf("op %q: param %q has invalid `in` %q (want path/query/body)", e.Name, p.Name, p.In)
		}
	}
	return o, nil
}

// extString reads an x- extension as a string. OpenAPI extensions arrive as
// any; accept a plain string or a JSON string. Returns "" when absent.
func extString(ext map[string]any, key string) string {
	v, ok := ext[key]
	if !ok || v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case json.RawMessage:
		var out string
		if json.Unmarshal(s, &out) == nil {
			return out
		}
	}
	return ""
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
