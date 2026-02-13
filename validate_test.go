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

func intPtr(n int) *int {
	return &n
}
