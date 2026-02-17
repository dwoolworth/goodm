package goodm

import (
	"testing"
)

func TestValidate_StringMinLength(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Name", BSONName: "name", Min: intPtr(3)},
		},
	}

	type model struct {
		Name string
	}

	// Too short
	errs := Validate(&model{Name: "ab"}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if errs[0].Field != "name" {
		t.Fatalf("expected field 'name', got %q", errs[0].Field)
	}

	// Exactly at minimum
	errs = Validate(&model{Name: "abc"}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}

	// Above minimum
	errs = Validate(&model{Name: "abcd"}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}
}

func TestValidate_StringMaxLength(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Name", BSONName: "name", Max: intPtr(5)},
		},
	}

	type model struct {
		Name string
	}

	// Within limit
	errs := Validate(&model{Name: "hello"}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}

	// Exceeds limit
	errs = Validate(&model{Name: "toolong"}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
}

func TestValidate_StringMinMaxCombined(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Name", BSONName: "name", Min: intPtr(2), Max: intPtr(10)},
		},
	}

	type model struct {
		Name string
	}

	// Too short
	errs := Validate(&model{Name: "a"}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error for too short, got %d", len(errs))
	}

	// Too long
	errs = Validate(&model{Name: "12345678901"}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error for too long, got %d", len(errs))
	}

	// Just right
	errs = Validate(&model{Name: "hello"}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}
}

func TestValidate_IntMinMax(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Age", BSONName: "age", Min: intPtr(0), Max: intPtr(200)},
		},
	}

	type model struct {
		Age int
	}

	// Below min (non-zero)
	errs := Validate(&model{Age: -1}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}

	// Above max
	errs = Validate(&model{Age: 201}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}

	// Valid
	errs = Validate(&model{Age: 25}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}
}

func TestValidate_Enum(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Role", BSONName: "role", Enum: []string{"admin", "user", "mod"}},
		},
	}

	type model struct {
		Role string
	}

	// Valid
	errs := Validate(&model{Role: "admin"}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}

	// Invalid
	errs = Validate(&model{Role: "superadmin"}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
}

func TestValidate_Required(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Email", BSONName: "email", Required: true},
		},
	}

	type model struct {
		Email string
	}

	// Missing
	errs := Validate(&model{Email: ""}, schema)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}

	// Present
	errs = Validate(&model{Email: "a@b.com"}, schema)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}
}

// --- subdocument validation tests ---

func TestValidate_SubdocumentRequired(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	// Empty address street should error with dotted path
	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "",
			City:   "NYC",
		},
	}
	errs := Validate(order, schema)

	found := false
	for _, e := range errs {
		if e.Field == "address.street" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected validation error on 'address.street', got %v", errs)
	}
}

func TestValidate_SubdocumentSliceRequired(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	// Empty item name in slice should error with indexed dotted path
	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "123 Main",
			City:   "NYC",
		},
		Items: []testOrderItem{
			{Name: "", Quantity: 2},
		},
	}
	errs := Validate(order, schema)

	found := false
	for _, e := range errs {
		if e.Field == "items[0].name" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected validation error on 'items[0].name', got %v", errs)
	}
}

func TestValidate_SubdocumentSliceMin(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	// Quantity below min=1 should error
	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "123 Main",
			City:   "NYC",
		},
		Items: []testOrderItem{
			{Name: "Widget", Quantity: 0}, // zero skipped for min check (IsZero)
			{Name: "Gadget", Quantity: -1},
		},
	}
	errs := Validate(order, schema)

	found := false
	for _, e := range errs {
		if e.Field == "items[1].quantity" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected validation error on 'items[1].quantity', got %v", errs)
	}
}

func TestValidate_SubdocumentZeroStructRequired(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	// Zero-value address with required tag should error on "address"
	order := &testOrder{
		Name: "Order1",
	}
	errs := Validate(order, schema)

	found := false
	for _, e := range errs {
		if e.Field == "address" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected validation error on 'address', got %v", errs)
	}
}

func TestValidate_SubdocumentAllValid(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "123 Main",
			City:   "NYC",
			Zip:    "10001",
		},
		Items: []testOrderItem{
			{Name: "Widget", Quantity: 5},
		},
	}
	errs := Validate(order, schema)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidate_SubdocumentEmptySlice(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testOrder")

	// Empty items slice — no inner validation errors
	order := &testOrder{
		Name: "Order1",
		Address: testAddress{
			Street: "123 Main",
			City:   "NYC",
		},
	}
	errs := Validate(order, schema)
	// Should only have errors from address sub-fields, not from items
	for _, e := range errs {
		if e.Field == "items" || len(e.Field) > 5 && e.Field[:5] == "items" {
			t.Fatalf("unexpected items error with empty slice: %v", e)
		}
	}
}

func TestValidate_DeeplyNested(t *testing.T) {
	// Test subdoc within subdoc using manually constructed schema
	type Inner struct {
		Value string `bson:"value" goodm:"required"`
	}
	type Outer struct {
		Inner Inner `bson:"inner"`
	}
	type Root struct {
		Outer Outer `bson:"outer"`
	}

	schema := &Schema{
		Fields: []FieldSchema{
			{
				Name:     "Outer",
				BSONName: "outer",
				SubFields: []FieldSchema{
					{
						Name:     "Inner",
						BSONName: "inner",
						SubFields: []FieldSchema{
							{Name: "Value", BSONName: "value", Required: true},
						},
					},
				},
			},
		},
	}

	root := &Root{}
	errs := Validate(root, schema)

	found := false
	for _, e := range errs {
		if e.Field == "outer.inner.value" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error on 'outer.inner.value', got %v", errs)
	}
}

func intPtr(n int) *int {
	return &n
}
