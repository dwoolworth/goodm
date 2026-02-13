package goodm

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// --- unit tests (no DB) ---

func TestRegister_Duplicate(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	// Second registration of the same model should error
	err := Register(&testUser{}, "test_users")
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestGetSchemaForModel(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	// Pointer
	s, err := getSchemaForModel(&testUser{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Collection != "test_users" {
		t.Fatalf("expected test_users, got %s", s.Collection)
	}

	// Slice element
	s, err = getSchemaForModel(&[]testUser{})
	if err != nil {
		t.Fatalf("unexpected error for slice: %v", err)
	}
	if s.Collection != "test_users" {
		t.Fatalf("expected test_users for slice, got %s", s.Collection)
	}

	// Unregistered
	type unknown struct{ Model }
	_, err = getSchemaForModel(&unknown{})
	if err == nil {
		t.Fatal("expected error for unregistered model")
	}
}

func TestGetModelID(t *testing.T) {
	id := bson.NewObjectID()
	u := &testUser{}
	u.ID = id

	got, err := getModelID(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != id {
		t.Fatalf("expected %s, got %s", id.Hex(), got.Hex())
	}
}

func TestSetModelID(t *testing.T) {
	u := &testUser{}
	id := bson.NewObjectID()
	setModelID(u, id)

	if u.ID != id {
		t.Fatalf("expected %s, got %s", id.Hex(), u.ID.Hex())
	}
}

func TestSetTimestamps(t *testing.T) {
	u := &testUser{}
	setTimestamps(u, fixedTime)

	if u.CreatedAt != fixedTime {
		t.Fatal("CreatedAt not set")
	}
	if u.UpdatedAt != fixedTime {
		t.Fatal("UpdatedAt not set")
	}

	// CreatedAt should not be overwritten
	setTimestamps(u, fixedTime.Add(1))
	if u.CreatedAt != fixedTime {
		t.Fatal("CreatedAt was overwritten")
	}
}

func TestValidateImmutable(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	schema, _ := Get("testUser")

	old := &testUser{Name: "Alice"}
	new := &testUser{Name: "Bob"}

	errs := validateImmutable(old, new, schema)
	if len(errs) == 0 {
		t.Fatal("expected immutable violation")
	}
	if errs[0].Field != "name" {
		t.Fatalf("expected name field, got %s", errs[0].Field)
	}

	// Same value should pass
	new.Name = "Alice"
	errs = validateImmutable(old, new, schema)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestGetDB_NilFallback(t *testing.T) {
	dbMu.Lock()
	saved := globalDB
	globalDB = nil
	dbMu.Unlock()
	defer func() {
		dbMu.Lock()
		globalDB = saved
		dbMu.Unlock()
	}()

	_, err := getDB(nil)
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("expected ErrNoDatabase, got %v", err)
	}
}

// --- integration tests (require MongoDB) ---

func TestCreate_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{
		Email: "alice@test.com",
		Name:  "Alice",
		Age:   30,
		Role:  "user",
	}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID.IsZero() {
		t.Fatal("ID should be set")
	}
	if u.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set")
	}
	if u.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be set")
	}
}

func TestCreate_ValidationFailure(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{
		Email: "", // required
		Name:  "Alice",
		Role:  "user",
	}
	err := Create(ctx, u)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
	}
}

func TestFindOne_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{Email: "find@test.com", Name: "Find", Age: 25, Role: "user"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	found := &testUser{}
	if err := FindOne(ctx, bson.D{{Key: "email", Value: "find@test.com"}}, found); err != nil {
		t.Fatalf("find one: %v", err)
	}
	if found.Name != "Find" {
		t.Fatalf("expected Find, got %s", found.Name)
	}
}

func TestFindOne_NotFound(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	err := FindOne(ctx, bson.D{{Key: "email", Value: "nonexistent"}}, &testUser{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFind_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		u := &testUser{
			Email: bson.NewObjectID().Hex() + "@test.com",
			Name:  "User",
			Age:   20 + i,
			Role:  "user",
		}
		if err := Create(ctx, u); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	var users []testUser
	if err := Find(ctx, bson.D{}, &users); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}
}

func TestFind_WithOptions(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	for i := 0; i < 5; i++ {
		u := &testUser{
			Email: bson.NewObjectID().Hex() + "@test.com",
			Name:  "User",
			Age:   20 + i,
			Role:  "user",
		}
		if err := Create(ctx, u); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	var users []testUser
	err := Find(ctx, bson.D{}, &users, FindOptions{
		Limit: 2,
		Sort:  bson.D{{Key: "age", Value: -1}},
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].Age < users[1].Age {
		t.Fatal("expected descending sort by age")
	}
}

func TestUpdate_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{Email: "update@test.com", Name: "Update", Age: 25, Role: "user"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	u.Age = 26
	if err := Update(ctx, u); err != nil {
		t.Fatalf("update: %v", err)
	}

	found := &testUser{}
	if err := FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, found); err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.Age != 26 {
		t.Fatalf("expected age 26, got %d", found.Age)
	}
}

func TestUpdate_ImmutableViolation(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{Email: "immut@test.com", Name: "Original", Age: 25, Role: "user"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	u.Name = "Changed"
	err := Update(ctx, u)
	if err == nil {
		t.Fatal("expected immutable error")
	}
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
}

func TestDelete_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{Email: "delete@test.com", Name: "Delete", Age: 25, Role: "user"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := Delete(ctx, u); err != nil {
		t.Fatalf("delete: %v", err)
	}

	err := FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, &testUser{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestUpdateOne_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{Email: "uone@test.com", Name: "UOne", Age: 25, Role: "user"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := UpdateOne(ctx,
		bson.D{{Key: "email", Value: "uone@test.com"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 99}}}},
		&testUser{},
	)
	if err != nil {
		t.Fatalf("update one: %v", err)
	}

	found := &testUser{}
	if err := FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, found); err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.Age != 99 {
		t.Fatalf("expected age 99, got %d", found.Age)
	}
}

func TestDeleteOne_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testUser{Email: "done@test.com", Name: "DOne", Age: 25, Role: "user"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := DeleteOne(ctx, bson.D{{Key: "email", Value: "done@test.com"}}, &testUser{})
	if err != nil {
		t.Fatalf("delete one: %v", err)
	}

	err = FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, &testUser{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestHooks_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := &testHookUser{Email: "hooks@test.com", Name: "Hooks"}
	if err := Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	if len(u.Events) < 2 || u.Events[0] != "before_create" || u.Events[1] != "after_create" {
		t.Fatalf("expected [before_create, after_create], got %v", u.Events)
	}

	// Reload and update to trigger save hooks
	found := &testHookUser{}
	if err := FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, found); err != nil {
		t.Fatalf("find: %v", err)
	}
	found.Events = nil // clear persisted events from DB
	found.Email = "hooks2@test.com"
	if err := Update(ctx, found); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(found.Events) < 2 || found.Events[0] != "before_save" || found.Events[1] != "after_save" {
		t.Fatalf("expected [before_save, after_save], got %v", found.Events)
	}

	// Delete hooks
	found.Events = nil
	if err := Delete(ctx, found); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(found.Events) < 2 || found.Events[0] != "before_delete" || found.Events[1] != "after_delete" {
		t.Fatalf("expected [before_delete, after_delete], got %v", found.Events)
	}
}

func TestMiddleware_WithCRUD_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	var ops []OpType
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		ops = append(ops, op.Operation)
		return next(ctx)
	})

	u := &testUser{Email: "mw@test.com", Name: "MW", Age: 25, Role: "user"}
	_ = Create(ctx, u)
	_ = FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, &testUser{})
	_ = Delete(ctx, u)

	expected := []OpType{OpCreate, OpFind, OpDelete}
	if len(ops) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, ops)
	}
	for i, v := range expected {
		if ops[i] != v {
			t.Fatalf("expected %v at index %d, got %v", v, i, ops[i])
		}
	}
}
