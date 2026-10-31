package api_test

import (
	"testing"
	"time"

	"gamebackend/pkg/api"
)

type nested struct {
	ID string `json:"id"`
}

type sample struct {
	Name    string    `json:"name" doc:"the name"`
	Hidden  string    `json:"-"`
	private string    //nolint:unused // proves unexported fields are skipped
	Opt     int64     `json:"opt,omitempty"`
	When    time.Time `json:"when"`
	Tags    []string  `json:"tags"`
	Child   nested    `json:"child"`
	Count   int32     `json:"count"`
	Ratio   float64   `json:"ratio"`
	Flag    bool      `json:"flag"`
}

func TestShapeOfStruct(t *testing.T) {
	shape := api.T(sample{})
	if shape.Type != "object" {
		t.Fatalf("type = %q, want object", shape.Type)
	}
	if shape.Name != "sample" {
		t.Fatalf("name = %q, want sample", shape.Name)
	}

	fields := map[string]api.Field{}
	for _, f := range shape.Fields {
		fields[f.Name] = f
	}
	for _, skipped := range []string{"-", "private"} {
		if _, ok := fields[skipped]; ok {
			t.Fatalf("field %q must be skipped", skipped)
		}
	}
	if got := len(shape.Fields); got != 8 {
		t.Fatalf("field count = %d, want 8", got)
	}
	if !fields["name"].Required || fields["name"].Doc != "the name" {
		t.Fatalf("name field = %+v, want required with doc", fields["name"])
	}
	if fields["opt"].Required {
		t.Fatalf("opt must be optional because of omitempty")
	}
	if fields["when"].Schema.Format != "date-time" {
		t.Fatalf("time.Time format = %q, want date-time", fields["when"].Schema.Format)
	}
	if fields["tags"].Schema.Type != "array" || fields["tags"].Schema.Items.Type != "string" {
		t.Fatalf("tags = %+v, want array of string", fields["tags"].Schema)
	}
	if fields["child"].Schema.Name != "nested" {
		t.Fatalf("child ref = %q, want nested", fields["child"].Schema.Name)
	}
	if fields["count"].Schema.Format != "int32" || fields["ratio"].Schema.Format != "double" {
		t.Fatalf("numeric formats wrong: %+v %+v", fields["count"].Schema, fields["ratio"].Schema)
	}
}

func TestPathParams(t *testing.T) {
	op := api.Op{Path: "/v1/a/{one}/b/{two}"}
	got := op.PathParams()
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("PathParams = %v", got)
	}
}

func TestValidateRejectsDuplicateRoutes(t *testing.T) {
	ops := []api.Op{
		{ID: "a.one", Service: "a", Method: "GET", Path: "/x", Summary: "s", Response: api.T(struct{}{})},
		{ID: "a.two", Service: "a", Method: "GET", Path: "/x", Summary: "s", Response: api.T(struct{}{})},
	}
	if err := api.Validate(ops); err == nil {
		t.Fatal("Validate accepted two ops on the same route")
	}
}
