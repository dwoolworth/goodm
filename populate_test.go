package goodm

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPopulate_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	// Create a profile
	profile := &testProfile{Bio: "Hello world"}
	if err := Create(ctx, profile); err != nil {
		t.Fatalf("create profile: %v", err)
	}

	// Create a user referencing the profile
	user := &testUser{
		Email:     "pop@test.com",
		Name:      "Pop",
		Age:       25,
		Role:      "user",
		ProfileID: profile.ID,
	}
	if err := Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Populate the profile ref
	loadedProfile := &testProfile{}
	if err := Populate(ctx, user, Refs{"profile": loadedProfile}); err != nil {
		t.Fatalf("populate: %v", err)
	}

	if loadedProfile.Bio != "Hello world" {
		t.Fatalf("expected 'Hello world', got %q", loadedProfile.Bio)
	}
	if loadedProfile.ID != profile.ID {
		t.Fatal("populated profile ID doesn't match")
	}
}

func TestPopulate_ZeroRef(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	user := &testUser{
		Email: "noref@test.com",
		Name:  "NoRef",
		Age:   25,
		Role:  "user",
		// ProfileID is zero
	}
	if err := Create(ctx, user); err != nil {
		t.Fatalf("create: %v", err)
	}

	loadedProfile := &testProfile{}
	if err := Populate(ctx, user, Refs{"profile": loadedProfile}); err != nil {
		t.Fatalf("populate with zero ref should not error: %v", err)
	}
	if !loadedProfile.ID.IsZero() {
		t.Fatal("profile should not be populated for zero ref")
	}
}

func TestPopulate_NoRefTag(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	user := &testUser{
		Email: "noref@test.com",
		Name:  "NoRef",
		Age:   25,
		Role:  "user",
	}

	err := Populate(context.Background(), user, Refs{"email": &testProfile{}})
	if err == nil {
		t.Fatal("expected error for field without ref tag")
	}
}

func TestPopulate_InvalidField(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	user := &testUser{
		Email: "bad@test.com",
		Name:  "Bad",
		Age:   25,
		Role:  "user",
	}

	err := Populate(context.Background(), user, Refs{"nonexistent": &testProfile{}})
	if err == nil {
		t.Fatal("expected error for nonexistent field")
	}
}

func TestPopulate_DanglingRef(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	user := &testUser{
		Email:     "dangling@test.com",
		Name:      "Dangling",
		Age:       25,
		Role:      "user",
		ProfileID: bson.NewObjectID(), // references nonexistent doc
	}
	if err := Create(ctx, user); err != nil {
		t.Fatalf("create: %v", err)
	}

	loadedProfile := &testProfile{}
	if err := Populate(ctx, user, Refs{"profile": loadedProfile}); err != nil {
		t.Fatalf("populate dangling ref should not error: %v", err)
	}
	if !loadedProfile.ID.IsZero() {
		t.Fatal("profile should not be populated for dangling ref")
	}
}
