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

func TestBatchPopulate_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	// Create two profiles
	p1 := &testProfile{Bio: "Bio one"}
	p2 := &testProfile{Bio: "Bio two"}
	if err := Create(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}
	if err := Create(ctx, p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}

	// Create users referencing them
	users := []testUser{
		{Email: "a@test.com", Name: "A", Age: 20, Role: "user", ProfileID: p1.ID},
		{Email: "b@test.com", Name: "B", Age: 21, Role: "user", ProfileID: p2.ID},
		{Email: "c@test.com", Name: "C", Age: 22, Role: "user", ProfileID: p1.ID}, // duplicate ref
	}
	for i := range users {
		if err := Create(ctx, &users[i]); err != nil {
			t.Fatalf("create user %d: %v", i, err)
		}
	}

	// BatchPopulate profiles
	var profiles []testProfile
	if err := BatchPopulate(ctx, users, "profile", &profiles); err != nil {
		t.Fatalf("batch populate: %v", err)
	}

	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}

	bios := map[string]bool{}
	for _, p := range profiles {
		bios[p.Bio] = true
	}
	if !bios["Bio one"] || !bios["Bio two"] {
		t.Fatalf("unexpected profiles: %v", profiles)
	}
}

func TestBatchPopulate_EmptySlice(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	var profiles []testProfile
	if err := BatchPopulate(context.Background(), []testUser{}, "profile", &profiles); err != nil {
		t.Fatalf("batch populate empty slice should not error: %v", err)
	}
}

func TestBatchPopulate_NoRefTag(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	users := []testUser{{Email: "a@test.com", Name: "A", Age: 20, Role: "user"}}
	var profiles []testProfile
	err := BatchPopulate(context.Background(), users, "email", &profiles)
	if err == nil {
		t.Fatal("expected error for field without ref tag")
	}
}

func TestBatchPopulate_AllZeroRefs(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []testUser{
		{Email: "a@test.com", Name: "A", Age: 20, Role: "user"},
		{Email: "b@test.com", Name: "B", Age: 21, Role: "user"},
	}
	for i := range users {
		if err := Create(ctx, &users[i]); err != nil {
			t.Fatalf("create user %d: %v", i, err)
		}
	}

	var profiles []testProfile
	if err := BatchPopulate(ctx, users, "profile", &profiles); err != nil {
		t.Fatalf("batch populate all-zero refs should not error: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("expected 0 profiles, got %d", len(profiles))
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
