package goodm_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/dwoolworth/goodm"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// User is an example model with schema tags for validation, uniqueness, and defaults.
type User struct {
	goodm.Model `bson:",inline"`
	Email       string `bson:"email" goodm:"unique,required"`
	Name        string `bson:"name"  goodm:"required,immutable"`
	Age         int    `bson:"age"   goodm:"min=13,max=120"`
	Role        string `bson:"role"  goodm:"enum=admin|user|mod,default=user"`
}

// Product demonstrates additional schema tags including default values and references.
type Product struct {
	goodm.Model `bson:",inline"`
	SKU         string        `bson:"sku"      goodm:"unique,required,immutable"`
	Name        string        `bson:"name"     goodm:"required"`
	Price       int           `bson:"price"    goodm:"min=0"`
	Category    string        `bson:"category" goodm:"index,enum=electronics|clothing|food"`
	Stock       int           `bson:"stock"    goodm:"default=0,min=0"`
	BrandID     bson.ObjectID `bson:"brand"    goodm:"ref=brands"`
}

// AuditableUser demonstrates lifecycle hooks for logging or side effects.
type AuditableUser struct {
	goodm.Model `bson:",inline"`
	Email       string `bson:"email" goodm:"unique,required"`
	Name        string `bson:"name"  goodm:"required"`
}

func (u *AuditableUser) BeforeCreate(ctx context.Context) error {
	fmt.Printf("Creating user: %s\n", u.Email)
	return nil
}

func (u *AuditableUser) AfterCreate(ctx context.Context) error {
	fmt.Printf("Created user: %s\n", u.Email)
	return nil
}

func init() {
	_ = goodm.Register(&User{}, "users")
	_ = goodm.Register(&Product{}, "products")
	_ = goodm.Register(&AuditableUser{}, "auditable_users")
}

// This example shows how to register a model and use schema tags to declare
// constraints. The struct definition is the database contract — tags like
// unique, required, immutable, min, max, enum, and default are enforced
// automatically on Create and Update.
func Example_schemaDefinition() {
	schema, ok := goodm.Get("User")
	if !ok {
		fmt.Println("User schema not found")
		return
	}
	fmt.Printf("Model: %s → collection: %s\n", schema.ModelName, schema.Collection)
	fmt.Printf("Fields: %d\n", len(schema.Fields))

	for _, f := range schema.Fields {
		var tags []string
		if f.Required {
			tags = append(tags, "required")
		}
		if f.Unique {
			tags = append(tags, "unique")
		}
		if f.Immutable {
			tags = append(tags, "immutable")
		}
		if f.Enum != nil {
			tags = append(tags, fmt.Sprintf("enum=%s", strings.Join(f.Enum, "|")))
		}
		if f.Default != "" {
			tags = append(tags, fmt.Sprintf("default=%s", f.Default))
		}
		if len(tags) > 0 {
			fmt.Printf("  %s: %s\n", f.BSONName, strings.Join(tags, ", "))
		}
	}

	// Output:
	// Model: User → collection: users
	// Fields: 8
	//   email: required, unique
	//   name: required, immutable
	//   role: enum=admin|user|mod, default=user
}

// This example shows how validation works. When you Create or Update a document,
// goodm automatically validates against the schema tags. Validation errors are
// returned as typed ValidationErrors you can inspect programmatically.
func Example_validation() {
	// A user with age below the minimum (13) and an invalid role
	user := &User{
		Email: "bob@example.com",
		Name:  "Bob",
		Age:   5,
		Role:  "superadmin",
	}

	schema, _ := goodm.Get("User")
	errs := goodm.Validate(user, schema)

	for _, e := range errs {
		fmt.Printf("%s: %s\n", e.Field, e.Message)
	}

	// Output:
	// age: value 5 is less than minimum 13
	// role: value "superadmin" is not in enum [admin user mod]
}

// This example shows how lifecycle hooks work. Implement BeforeCreate,
// AfterCreate, BeforeSave, AfterSave, BeforeDelete, or AfterDelete on your
// model struct. goodm detects them automatically — no registration needed.
func Example_hooks() {
	schema, _ := goodm.Get("AuditableUser")

	fmt.Printf("Model: %s\n", schema.ModelName)
	fmt.Printf("Hooks: %v\n", schema.Hooks)

	// Output:
	// Model: AuditableUser
	// Hooks: [BeforeCreate AfterCreate]
}

// This example shows the fluent aggregation pipeline builder.
// Pipelines are bound to a model for collection resolution.
func Example_pipeline() {
	// Build a pipeline (does not execute without a DB connection)
	p := goodm.NewPipeline(&User{}).
		Match(bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}}).
		Group(bson.D{
			{Key: "_id", Value: "$role"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}).
		Sort(bson.D{{Key: "count", Value: -1}}).
		Limit(10)

	fmt.Printf("Pipeline stages: %d\n", len(p.Stages()))

	// Output:
	// Pipeline stages: 4
}
