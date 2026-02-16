package goodm

import (
	"reflect"
	"testing"
)

type testDefaults struct {
	Model   `bson:",inline"`
	Name    string  `bson:"name"    goodm:"default=anonymous"`
	Age     int     `bson:"age"     goodm:"default=18"`
	Score   float64 `bson:"score"   goodm:"default=9.5"`
	Active  bool    `bson:"active"  goodm:"default=true"`
	NoDefault string `bson:"no_default"`
}

func TestApplyDefaults_String(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Name", BSONName: "name", Default: "anonymous"},
		},
	}

	m := &testDefaults{}
	if err := applyDefaults(m, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "anonymous" {
		t.Fatalf("expected 'anonymous', got %q", m.Name)
	}
}

func TestApplyDefaults_Int(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Age", BSONName: "age", Default: "18"},
		},
	}

	m := &testDefaults{}
	if err := applyDefaults(m, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Age != 18 {
		t.Fatalf("expected 18, got %d", m.Age)
	}
}

func TestApplyDefaults_Float64(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Score", BSONName: "score", Default: "9.5"},
		},
	}

	m := &testDefaults{}
	if err := applyDefaults(m, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Score != 9.5 {
		t.Fatalf("expected 9.5, got %f", m.Score)
	}
}

func TestApplyDefaults_Bool(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Active", BSONName: "active", Default: "true"},
		},
	}

	m := &testDefaults{}
	if err := applyDefaults(m, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.Active {
		t.Fatal("expected Active to be true")
	}
}

func TestApplyDefaults_NonZeroNotOverwritten(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Name", BSONName: "name", Default: "anonymous"},
			{Name: "Age", BSONName: "age", Default: "18"},
		},
	}

	m := &testDefaults{Name: "Alice", Age: 30}
	if err := applyDefaults(m, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "Alice" {
		t.Fatalf("expected 'Alice', got %q", m.Name)
	}
	if m.Age != 30 {
		t.Fatalf("expected 30, got %d", m.Age)
	}
}

func TestApplyDefaults_InvalidParse(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Age", BSONName: "age", Default: "not_a_number"},
		},
	}

	m := &testDefaults{}
	err := applyDefaults(m, schema)
	if err == nil {
		t.Fatal("expected error for invalid int parse")
	}
}

func TestSetFieldFromString_UnsupportedType(t *testing.T) {
	// A slice field cannot be set from string
	v := reflect.ValueOf(&[]string{}).Elem()
	err := setFieldFromString(v, "test")
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
}
