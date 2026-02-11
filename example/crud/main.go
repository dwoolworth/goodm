package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/dwoolworth/goodm"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// User demonstrates a model with hooks, immutable fields, and validation.
type User struct {
	goodm.Model `bson:",inline"`
	Email       string `bson:"email" goodm:"unique,required"`
	Name        string `bson:"name"  goodm:"required,immutable"`
	Age         int    `bson:"age"   goodm:"min=13,max=120"`
}

func (u *User) BeforeCreate(ctx context.Context) error {
	fmt.Println("  [hook] BeforeCreate fired")
	return nil
}

func (u *User) AfterCreate(ctx context.Context) error {
	fmt.Println("  [hook] AfterCreate fired")
	return nil
}

func (u *User) BeforeSave(ctx context.Context) error {
	fmt.Println("  [hook] BeforeSave fired")
	return nil
}

func (u *User) AfterSave(ctx context.Context) error {
	fmt.Println("  [hook] AfterSave fired")
	return nil
}

func (u *User) BeforeDelete(ctx context.Context) error {
	fmt.Println("  [hook] BeforeDelete fired")
	return nil
}

func (u *User) AfterDelete(ctx context.Context) error {
	fmt.Println("  [hook] AfterDelete fired")
	return nil
}

func init() {
	if err := goodm.Register(&User{}, "crud_example_users"); err != nil {
		panic(err)
	}
}

func main() {
	ctx := context.Background()

	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	dbName := os.Getenv("MONGODB_DB")
	if dbName == "" {
		dbName = "goodm_crud_example"
	}

	// 1. Connect + Enforce
	fmt.Println("=== Connect & Enforce ===")
	db, err := goodm.Connect(ctx, uri, dbName)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	if err := goodm.Enforce(ctx, db); err != nil {
		log.Fatalf("enforce: %v", err)
	}
	fmt.Println("Connected and enforced schemas")

	// Clean up collection for a fresh demo
	_ = db.Collection("crud_example_users").Drop(ctx)

	// 2. Create a user
	fmt.Println("\n=== Create ===")
	user := &User{
		Email: "alice@example.com",
		Name:  "Alice",
		Age:   30,
	}
	if err := goodm.Create(ctx, user); err != nil {
		log.Fatalf("create: %v", err)
	}
	fmt.Printf("Created user: ID=%s, CreatedAt=%s\n", user.ID.Hex(), user.CreatedAt.Format(time.RFC3339))

	// 3. FindOne by email
	fmt.Println("\n=== FindOne ===")
	found := &User{}
	if err := goodm.FindOne(ctx, bson.D{{Key: "email", Value: "alice@example.com"}}, found); err != nil {
		log.Fatalf("find one: %v", err)
	}
	fmt.Printf("Found user: %s (%s), age %d\n", found.Name, found.Email, found.Age)

	// 4. Update age (allowed — age is not immutable)
	fmt.Println("\n=== Update (age) ===")
	found.Age = 31
	if err := goodm.Update(ctx, found); err != nil {
		log.Fatalf("update age: %v", err)
	}
	fmt.Printf("Updated age to %d, UpdatedAt=%s\n", found.Age, found.UpdatedAt.Format(time.RFC3339))

	// 5. Attempt immutable field change (name)
	fmt.Println("\n=== Update (immutable name — expect error) ===")
	found.Name = "Bob"
	if err := goodm.Update(ctx, found); err != nil {
		fmt.Printf("Expected error: %v\n", err)
		found.Name = "Alice" // revert
	} else {
		log.Fatal("expected immutable error but got none")
	}

	// 6. Find all users
	fmt.Println("\n=== Find All ===")
	// Create a second user
	user2 := &User{Email: "bob@example.com", Name: "Bob", Age: 25}
	if err := goodm.Create(ctx, user2); err != nil {
		log.Fatalf("create user2: %v", err)
	}
	var users []User
	if err := goodm.Find(ctx, bson.D{}, &users); err != nil {
		log.Fatalf("find all: %v", err)
	}
	fmt.Printf("Found %d users:\n", len(users))
	for _, u := range users {
		fmt.Printf("  - %s (%s), age %d\n", u.Name, u.Email, u.Age)
	}

	// 7. Delete user and verify not found
	fmt.Println("\n=== Delete ===")
	if err := goodm.Delete(ctx, found); err != nil {
		log.Fatalf("delete: %v", err)
	}
	fmt.Println("Deleted Alice")

	err = goodm.FindOne(ctx, bson.D{{Key: "_id", Value: found.ID}}, &User{})
	if err == goodm.ErrNotFound {
		fmt.Println("Verified: Alice not found (ErrNotFound)")
	} else if err != nil {
		log.Fatalf("unexpected error: %v", err)
	} else {
		log.Fatal("expected ErrNotFound but found document")
	}

	// Cleanup
	_ = db.Collection("crud_example_users").Drop(ctx)
	fmt.Println("\n=== Done ===")
}
