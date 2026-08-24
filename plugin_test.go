package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/opentalon/pkg/plugin"
)

const testSpec = `
{
  "openapi": "3.0.0",
  "info": {
    "title": "t",
    "version": "1"
  },
  "paths": {
    "/api/v1/ticket_templates": {
      "get": {
        "summary": "lists ticket templates",
        "parameters": [
          {
            "name": "query",
            "in": "query",
            "schema": {
              "type": "string"
            }
          }
        ]
      }
    },
    "/api/v1/tickets/{id}": {
      "patch": {
        "summary": "updates a ticket",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": {
              "type": "integer"
            }
          }
        ],
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "required": [
                  "name"
                ],
                "properties": {
                  "name": {
                    "type": "string"
                  },
                  "ticket_template_id": {
                    "type": "integer"
                  }
                }
              }
            }
          }
        }
      }
    },
    "/api/v1/items/{id}/checkout": {
      "post": {
        "summary": "checkout item",
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": {
              "type": "integer"
            }
          }
        ],
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "person_id": {
                    "type": "integer"
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}
`

// gotRequest captures what the fake API received.
type gotRequest struct {
	method string
	path   string
	query  string
	header string
	body   map[string]any
}

func newServer(t *testing.T, capture *gotRequest) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/spec", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, testSpec)
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		capture.method = r.Method
		capture.path = r.URL.Path
		capture.query = r.URL.RawQuery
		capture.header = r.Header.Get("X-Timly-User-Id")
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &capture.body)
		}
		_, _ = io.WriteString(w, `{"id":7,"ok":true}`)
	})
	return httptest.NewServer(mux)
}

func configured(t *testing.T, srv *httptest.Server) *handler {
	t.Helper()
	h := &handler{}
	cfg, _ := json.Marshal(Config{
		ServerName: "timly-api",
		SpecURL:    srv.URL + "/spec",
		BaseURL:    srv.URL,
		Headers:    map[string]string{"X-Service": "svc-token"},
	})
	if err := h.Configure(string(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return h
}

func TestCapabilities_SynthesisedNames(t *testing.T) {
	srv := newServer(t, &gotRequest{})
	defer srv.Close()
	h := configured(t, srv)

	caps := h.Capabilities()
	if caps.Name != "timly-api" {
		t.Errorf("server name: got %q, want timly-api", caps.Name)
	}
	want := map[string]bool{"list_ticket_templates": false, "update_ticket": false, "checkout_item": false}
	for _, a := range caps.Actions {
		if _, ok := want[a.Name]; ok {
			want[a.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing synthesised action %q", name)
		}
	}
	// update_ticket must be a write (not ReadOnly); GET list must be ReadOnly.
	for _, a := range caps.Actions {
		if a.Name == "list_ticket_templates" && !a.ReadOnly {
			t.Errorf("list_ticket_templates should be ReadOnly")
		}
		if a.Name == "update_ticket" && a.ReadOnly {
			t.Errorf("update_ticket should not be ReadOnly")
		}
	}
}

func TestExecute_BuildsRequestAndForwardsIdentity(t *testing.T) {
	var got gotRequest
	srv := newServer(t, &got)
	defer srv.Close()
	h := configured(t, srv)

	resp := h.Execute(plugin.Request{
		ID:     "1",
		Action: "update_ticket",
		Args:   map[string]string{"id": "7", "name": "x", "ticket_template_id": "3"},
		CredentialHeaders: map[string]plugin.CredentialHeader{
			"timly": {Header: "X-Timly-User-Id", Value: "2383"},
		},
	})

	if resp.Error != "" {
		t.Fatalf("Execute error: %s", resp.Error)
	}
	if resp.StructuredContent != `{"id":7,"ok":true}` {
		t.Errorf("structured content: got %q", resp.StructuredContent)
	}
	if got.method != http.MethodPatch || got.path != "/api/v1/tickets/7" {
		t.Errorf("request line: got %s %s, want PATCH /api/v1/tickets/7", got.method, got.path)
	}
	if got.header != "2383" {
		t.Errorf("identity header X-Timly-User-Id: got %q, want 2383", got.header)
	}
	// integer body prop must serialise as a number, not a string.
	if v, ok := got.body["ticket_template_id"].(float64); !ok || v != 3 {
		t.Errorf("ticket_template_id: got %v (%T), want number 3", got.body["ticket_template_id"], got.body["ticket_template_id"])
	}
	if got.body["name"] != "x" {
		t.Errorf("name: got %v, want x", got.body["name"])
	}
}

func TestExecute_UnknownOperation(t *testing.T) {
	srv := newServer(t, &gotRequest{})
	defer srv.Close()
	h := configured(t, srv)

	resp := h.Execute(plugin.Request{ID: "1", Action: "nope"})
	if resp.Error == "" {
		t.Fatal("expected error for unknown operation")
	}
}

func TestSynthName(t *testing.T) {
	cases := map[string][2]string{
		"list_ticket_templates": {"get", "/api/v1/ticket_templates"},
		"show_ticket":           {"get", "/api/v1/tickets/{id}"},
		"create_ticket":         {"post", "/api/v1/tickets"},
		"update_ticket":         {"patch", "/api/v1/tickets/{id}"},
		"delete_ticket":         {"delete", "/api/v1/tickets/{id}"},
		"checkout_item":         {"post", "/api/v1/items/{id}/checkout"},
	}
	for want, mp := range cases {
		if got := synthName(mp[0], mp[1]); got != want {
			t.Errorf("synthName(%s, %s) = %q, want %q", mp[0], mp[1], got, want)
		}
	}
}
