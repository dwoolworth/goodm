package goodm

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var fixedTime = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func TestCreateMany_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []testUser{
		{Email: "bulk1@test.com", Name: "Bulk1", Age: 20, Role: "user"},
		{Email: "bulk2@test.com", Name: "Bulk2", Age: 21, Role: "user"},
		{Email: "bulk3@test.com", Name: "Bulk3", Age: 22, Role: "admin"},
	}

	if err := CreateMany(ctx, users); err != nil {
		t.Fatalf("create many: %v", err)
	}

	// Verify all were created with IDs and timestamps
	for i, u := range users {
		if u.ID.IsZero() {
			t.Fatalf("user %d: ID not set", i)
		}
		if u.CreatedAt.IsZero() {
			t.Fatalf("user %d: CreatedAt not set", i)
		}
	}

	// Verify in DB
	var found []testUser
	if err := Find(ctx, bson.D{}, &found); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("expected 3 users in DB, got %d", len(found))
	}
}

func TestCreateMany_WithPointers(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []*testUser{
		{Email: "ptr1@test.com", Name: "Ptr1", Age: 20, Role: "user"},
		{Email: "ptr2@test.com", Name: "Ptr2", Age: 21, Role: "user"},
	}

	if err := CreateMany(ctx, users); err != nil {
		t.Fatalf("create many ptrs: %v", err)
	}

	for i, u := range users {
		if u.ID.IsZero() {
			t.Fatalf("user %d: ID not set", i)
		}
	}
}

func TestCreateMany_Empty(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	var users []testUser
	if err := CreateMany(ctx, users); err != nil {
		t.Fatalf("create many empty should not error: %v", err)
	}
}

func TestCreateMany_ValidationFailure(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []testUser{
		{Email: "ok@test.com", Name: "OK", Age: 20, Role: "user"},
		{Email: "", Name: "Bad", Age: 20, Role: "user"}, // missing required email
	}

	err := CreateMany(ctx, users)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestCreateMany_Hooks(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []testHookUser{
		{Email: "hook1@test.com", Name: "Hook1"},
		{Email: "hook2@test.com", Name: "Hook2"},
	}

	if err := CreateMany(ctx, users); err != nil {
		t.Fatalf("create many hooks: %v", err)
	}

	for i, u := range users {
		if len(u.Events) == 0 {
			t.Fatalf("user %d: no hook events recorded", i)
		}
		if u.Events[0] != "before_create" {
			t.Fatalf("user %d: expected before_create, got %v", i, u.Events)
		}
	}
}

func TestUpdateMany_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []testUser{
		{Email: "um1@test.com", Name: "UM1", Age: 20, Role: "user"},
		{Email: "um2@test.com", Name: "UM2", Age: 21, Role: "user"},
		{Email: "um3@test.com", Name: "UM3", Age: 22, Role: "admin"},
	}
	if err := CreateMany(ctx, users); err != nil {
		t.Fatalf("create many: %v", err)
	}

	result, err := UpdateMany(ctx,
		bson.D{{Key: "role", Value: "user"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 99}}}},
		&testUser{},
	)
	if err != nil {
		t.Fatalf("update many: %v", err)
	}
	if result.MatchedCount != 2 {
		t.Fatalf("expected 2 matched, got %d", result.MatchedCount)
	}
	if result.ModifiedCount != 2 {
		t.Fatalf("expected 2 modified, got %d", result.ModifiedCount)
	}
}

func TestDeleteMany_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	users := []testUser{
		{Email: "dm1@test.com", Name: "DM1", Age: 20, Role: "user"},
		{Email: "dm2@test.com", Name: "DM2", Age: 21, Role: "user"},
		{Email: "dm3@test.com", Name: "DM3", Age: 22, Role: "admin"},
	}
	if err := CreateMany(ctx, users); err != nil {
		t.Fatalf("create many: %v", err)
	}

	result, err := DeleteMany(ctx,
		bson.D{{Key: "role", Value: "user"}},
		&testUser{},
	)
	if err != nil {
		t.Fatalf("delete many: %v", err)
	}
	if result.DeletedCount != 2 {
		t.Fatalf("expected 2 deleted, got %d", result.DeletedCount)
	}

	// Verify only admin remains
	var remaining []testUser
	if err := Find(ctx, bson.D{}, &remaining); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(remaining))
	}
	if remaining[0].Role != "admin" {
		t.Fatalf("expected admin, got %s", remaining[0].Role)
	}
}
