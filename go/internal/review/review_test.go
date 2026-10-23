package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The contract directory is shared with runner/src/schema.test.ts.
const contractDir = "../../../contract"

func TestContractExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(contractDir, "examples", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no contract examples: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		_, err = Parse(data)
		if want := strings.HasPrefix(name, "valid-"); (err == nil) != want {
			t.Errorf("%s: Parse error = %v, want valid = %v", name, err, want)
		}
	}
}

// jsonSchema is the subset of the generated JSON schema that Go mirrors by hand.
type jsonSchema struct {
	Type       string                `json:"type"`
	Required   []string              `json:"required"`
	Properties map[string]jsonSchema `json:"properties"`
	Items      *jsonSchema           `json:"items"`
	Enum       []string              `json:"enum"`
	Const      any                   `json:"const"`
	MaxItems   int                   `json:"maxItems"`
}

func loadSchema(t *testing.T) jsonSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractDir, "review-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonSchema
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func jsonFields(v any) []string {
	var names []string
	typ := reflect.TypeOf(v)
	for i := range typ.NumField() {
		names = append(names, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	slices.Sort(names)
	return names
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func strs[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

func TestSchemaMatchesGoTypes(t *testing.T) {
	s := loadSchema(t)
	comment := *s.Properties["comments"].Items
	objects := []struct {
		name   string
		schema jsonSchema
		goType any
	}{
		{"result", s, Result{}},
		{"pr", s.Properties["pr"], PR{}},
		{"comment", comment, Comment{}},
	}
	for _, o := range objects {
		// Every field is required, so required lists exactly the Go fields.
		if got, want := sorted(o.schema.Required), jsonFields(o.goType); !slices.Equal(got, want) {
			t.Errorf("%s fields: schema %v, Go %v", o.name, got, want)
		}
	}

	enums := []struct {
		name   string
		schema []string
		goVals []string
	}{
		{"verdict", s.Properties["verdict"].Enum, strs(Verdicts)},
		{"severity", comment.Properties["severity"].Enum, strs(Severities)},
		{"confidence", comment.Properties["confidence"].Enum, strs(Confidences)},
	}
	for _, e := range enums {
		if !slices.Equal(e.schema, e.goVals) {
			t.Errorf("%s: schema %v, Go %v", e.name, e.schema, e.goVals)
		}
	}
	if v, ok := s.Properties["schema_version"].Const.(float64); !ok || int(v) != SchemaVersion {
		t.Errorf("schema_version const %v, Go %d", s.Properties["schema_version"].Const, SchemaVersion)
	}
	if got := s.Properties["comments"].MaxItems; got != MaxComments {
		t.Errorf("comments maxItems %d, Go %d", got, MaxComments)
	}
}

func TestVerdictsMatchStore(t *testing.T) {
	sql, err := os.ReadFile("../store/migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := "verdict IN ('" + strings.Join(strs(Verdicts), "', '") + "')"
	if !strings.Contains(string(sql), want) {
		t.Errorf("migration does not contain %q", want)
	}
}
