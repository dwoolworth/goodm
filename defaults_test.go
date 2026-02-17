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

// --- subdocument defaults tests ---

func TestApplyDefaults_SubdocumentStruct(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "123 Main",
			City:   "NYC",
			// Zip left empty — should get default "00000"
		},
	}
	if err := applyDefaults(order, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.Address.Zip != "00000" {
		t.Fatalf("expected Zip '00000', got %q", order.Address.Zip)
	}
}

func TestApplyDefaults_SubdocumentStructNoOverwrite(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "123 Main",
			City:   "NYC",
			Zip:    "12345",
		},
	}
	if err := applyDefaults(order, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.Address.Zip != "12345" {
		t.Fatalf("expected Zip '12345' (not overwritten), got %q", order.Address.Zip)
	}
}

func TestApplyDefaults_SubdocumentSlice(t *testing.T) {
	// Use a manually constructed schema with defaults inside slice elements
	type Item struct {
		Name   string `bson:"name"`
		Status string `bson:"status" goodm:"default=pending"`
	}
	type Container struct {
		Items []Item `bson:"items"`
	}

	schema := &Schema{
		Fields: []FieldSchema{
			{
				Name:     "Items",
				BSONName: "items",
				IsSlice:  true,
				SubFields: []FieldSchema{
					{Name: "Name", BSONName: "name"},
					{Name: "Status", BSONName: "status", Default: "pending"},
				},
			},
		},
	}

	c := &Container{
		Items: []Item{
			{Name: "A"},           // Status empty — should get default
			{Name: "B", Status: "done"}, // Status set — should not overwrite
		},
	}
	if err := applyDefaults(c, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Items[0].Status != "pending" {
		t.Fatalf("expected Status 'pending', got %q", c.Items[0].Status)
	}
	if c.Items[1].Status != "done" {
		t.Fatalf("expected Status 'done' (not overwritten), got %q", c.Items[1].Status)
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
