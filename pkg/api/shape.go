package api

import (
	"reflect"
	"strings"
	"time"
)

// Shape is a transport-neutral description of a JSON payload.
//
// It is derived from Go types by reflection so that request/response structs
// and the published schema are the same thing. Keep tags honest: the json tag
// is the wire name, "doc" is the description and omitempty marks the field
// optional.
type Shape struct {
	Name                 string  // component name for objects; empty means inline
	Doc                  string  // optional component description
	Type                 string  // object | array | string | integer | number | boolean
	Format               string  // int64, int32, double, date-time
	Fields               []Field // when Type == "object"
	Items                *Shape  // when Type == "array"
	AdditionalProperties string  // when Type == "object" and it is a string map
	Nullable             bool
}

// Field is one object property.
type Field struct {
	Name     string
	Schema   Shape
	Doc      string
	Required bool
}

var timeType = reflect.TypeOf(time.Time{})

// T describes a struct value (pointer or not): the usual request/response body.
func T(v any) Shape { return shapeOf(reflect.TypeOf(v)) }

// Slice describes a JSON array of v.
func Slice(v any) Shape { return Shape{Type: "array", Items: ptr(shapeOf(reflect.TypeOf(v)))} }

func ptr(s Shape) *Shape { return &s }

func shapeOf(t reflect.Type) Shape {
	if t == nil {
		return Shape{Type: "object"}
	}
	if t.Kind() == reflect.Pointer {
		s := shapeOf(t.Elem())
		s.Nullable = true
		return s
	}
	if t == timeType {
		return Shape{Type: "string", Format: "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return Shape{Type: "string"}
	case reflect.Bool:
		return Shape{Type: "boolean"}
	case reflect.Int, reflect.Int32:
		return Shape{Type: "integer", Format: "int32"}
	case reflect.Int64:
		return Shape{Type: "integer", Format: "int64"}
	case reflect.Float32, reflect.Float64:
		return Shape{Type: "number", Format: "double"}
	case reflect.Slice, reflect.Array:
		return Shape{Type: "array", Items: ptr(shapeOf(t.Elem()))}
	case reflect.Map:
		if t.Elem().Kind() == reflect.String {
			return Shape{Type: "object", AdditionalProperties: "string"}
		}
		return Shape{Type: "object", AdditionalProperties: "string"}
	case reflect.Struct:
		return objectShape(t)
	case reflect.Interface:
		return Shape{Type: "object", AdditionalProperties: "string"}
	default:
		return Shape{Type: "string"}
	}
}

func objectShape(t reflect.Type) Shape {
	s := Shape{Name: t.Name(), Type: "object"}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := tag
		var optional bool
		if before, after, ok := strings.Cut(tag, ","); ok {
			name = before
			optional = strings.Contains(after, "omitempty")
		}
		if name == "" {
			name = sf.Name
		}
		s.Fields = append(s.Fields, Field{
			Name:     name,
			Schema:   shapeOf(sf.Type),
			Doc:      sf.Tag.Get("doc"),
			Required: !optional,
		})
	}
	return s
}

// Body returns a pointer to the shape of v, for Op.Request.
func Body(v any) *Shape { s := T(v); return &s }
