package api

import (
	"encoding/json"
	"sort"
	"strings"
)

// OpenAPIVersion is the published spec version. Bump only with an ADR.
const OpenAPIVersion = "3.1.0"

type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

type Parameter struct {
	Name     string         `json:"name"`
	In       string         `json:"in"`
	Required bool           `json:"required"`
	Schema   map[string]any `json:"schema"`
}

type MediaType struct {
	Schema any `json:"schema"`
}

type RequestBody struct {
	Required bool                 `json:"required"`
	Content  map[string]MediaType `json:"content"`
}

type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

type Operation struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary"`
	Tags        []string              `json:"tags"`
	Security    []map[string][]string `json:"security,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Idempotent  bool                  `json:"x-idempotent,omitempty"`
	Note        string                `json:"x-note,omitempty"`
}

type Components struct {
	Schemas         map[string]any               `json:"schemas"`
	SecuritySchemes map[string]map[string]string `json:"securitySchemes"`
}

// Document is a generated OpenAPI 3.1 document.
type Document struct {
	OpenAPI    string                          `json:"openapi"`
	Info       Info                            `json:"info"`
	Servers    []Server                        `json:"servers"`
	Paths      map[string]map[string]Operation `json:"paths"`
	Components Components                      `json:"components"`
}

// ErrorSchema is the schema shared by every error response.
var ErrorSchema = map[string]any{
	"type":     "object",
	"required": []string{"error"},
	"properties": map[string]any{
		"error": map[string]any{
			"type":     "object",
			"required": []string{"code", "message"},
			"properties": map[string]any{
				"code":    map[string]any{"type": "string"},
				"message": map[string]any{"type": "string"},
				"details": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
				},
			},
		},
	},
}

// BuildDocument turns operations into an OpenAPI 3.1 document.
func BuildDocument(title string, ops []Op) *Document {
	doc := &Document{
		OpenAPI: OpenAPIVersion,
		Info:    Info{Title: title, Version: "0.1.0", Description: "Generated from the Go contract declarations. Do not edit by hand."},
		Servers: []Server{{URL: "http://localhost:8080", Description: "local dev (single-process platform)"}},
		Paths:   map[string]map[string]Operation{},
		Components: Components{
			Schemas: map[string]any{"Error": ErrorSchema},
			SecuritySchemes: map[string]map[string]string{
				"playerToken":  {"type": "http", "scheme": "bearer", "description": "Player session token issued by the account service."},
				"serviceToken": {"type": "http", "scheme": "bearer", "description": "Internal service token. Never shipped to clients."},
			},
		},
	}
	c := &collector{schemas: doc.Components.Schemas}

	for _, op := range ops {
		if doc.Paths[op.Path] == nil {
			doc.Paths[op.Path] = map[string]Operation{}
		}
		doc.Paths[op.Path][strings.ToLower(op.Method)] = c.operation(op)
	}
	return doc
}

// Marshal renders the document as stable, indented JSON.
func (d *Document) Marshal() ([]byte, error) {
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, errf("marshal openapi: %w", err)
	}
	return append(body, '\n'), nil
}

type collector struct {
	schemas map[string]any
}

func (c *collector) operation(op Op) Operation {
	out := Operation{
		OperationID: op.ID,
		Summary:     op.Summary,
		Tags:        []string{op.Service},
		Responses:   map[string]Response{},
		Idempotent:  op.Idempotent,
		Note:        op.Note,
	}
	if scheme := op.Auth.SecurityScheme(); scheme != "" {
		out.Security = []map[string][]string{{scheme: {}}}
	}
	for _, name := range op.PathParams() {
		out.Parameters = append(out.Parameters, Parameter{
			Name:     name,
			In:       "path",
			Required: true,
			Schema:   map[string]any{"type": "string"},
		})
	}
	if op.Request != nil {
		out.RequestBody = &RequestBody{
			Required: true,
			Content:  map[string]MediaType{"application/json": {Schema: c.component(*op.Request)}},
		}
	}
	out.Responses["200"] = Response{
		Description: "OK",
		Content:     map[string]MediaType{"application/json": {Schema: c.component(op.Response)}},
	}
	out.Responses["default"] = Response{
		Description: "Error envelope. See components.schemas.Error.",
		Content:     map[string]MediaType{"application/json": {Schema: map[string]any{"$ref": "#/components/schemas/Error"}}},
	}
	return out
}

func (c *collector) component(s Shape) any {
	if s.Nullable {
		inner := s
		inner.Nullable = false
		return map[string]any{"anyOf": []any{c.component(inner), map[string]any{"type": "null"}}}
	}
	if s.Type == "object" && s.Name != "" && s.AdditionalProperties == "" && len(s.Fields) > 0 {
		if _, ok := c.schemas[s.Name]; !ok {
			c.schemas[s.Name] = c.inlineObject(s)
		}
		return map[string]any{"$ref": "#/components/schemas/" + s.Name}
	}
	if s.Type == "array" {
		items := any(map[string]any{"type": "string"})
		if s.Items != nil {
			items = c.component(*s.Items)
		}
		return map[string]any{"type": "array", "items": items}
	}
	if s.Type == "object" {
		if s.AdditionalProperties != "" {
			return map[string]any{"type": "object", "additionalProperties": map[string]any{"type": s.AdditionalProperties}}
		}
		return c.inlineObject(s)
	}
	out := map[string]any{"type": s.Type}
	if s.Format != "" {
		out["format"] = s.Format
	}
	return out
}

func (c *collector) inlineObject(s Shape) map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, f := range s.Fields {
		schema := c.component(f.Schema)
		if m, ok := schema.(map[string]any); ok && f.Doc != "" {
			withDoc := make(map[string]any, len(m)+1)
			for k, v := range m {
				withDoc[k] = v
			}
			withDoc["description"] = f.Doc
			schema = withDoc
		}
		props[f.Name] = schema
		if f.Required {
			required = append(required, f.Name)
		}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	if s.Doc != "" {
		out["description"] = s.Doc
	}
	return out
}
