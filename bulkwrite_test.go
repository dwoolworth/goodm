package goodm

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestBulk_MixedOperations(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	seedUsers(t, ctx, 3) // u0 admin, u1 user, u2 admin

	res, err := NewBulk(&testUser{}).
		InsertOne(&testUser{Email: "bulk@test.com", Name: "Bulk", Role: "user"}).
		UpdateOne(bson.D{{Key: "email", Value: "u1@test.com"}}, bson.M{"$set": bson.M{"age": 77}}).
		UpdateOne(bson.D{{Key: "email", Value: "upsert@test.com"}}, bson.M{"$set": bson.M{"name": "Up"}}, Upsert()).
		UpdateMany(bson.D{{Key: "role", Value: "admin"}}, bson.M{"$set": bson.M{"age": 1}}).
		DeleteOne(bson.D{{Key: "email", Value: "u0@test.com"}}).
		Execute(ctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.InsertedCount != 1 || res.UpsertedCount != 1 || res.DeletedCount != 1 || res.MatchedCount != 3 {
		t.Errorf("unexpected result: %+v", res)
	}
	if _, ok := res.UpsertedIDs[2]; !ok {
		t.Errorf("expected upserted id for op 2: %+v", res.UpsertedIDs)
	}
	n, _ := Count(ctx, bson.D{}, &testUser{})
	if n != 4 {
		t.Errorf("expected 4 documents, got %d", n)
	}

	empty, err := NewBulk(&testUser{}).Execute(ctx)
	if err != nil || empty == nil {
		t.Errorf("empty bulk: %+v %v", empty, err)
	}
}

func TestBulk_UnorderedReportsFailedIndexes(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	seedUsers(t, ctx, 1)

	var mw []OpType
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		mw = append(mw, op.Operation)
		return next(ctx)
	})

	res, err := NewBulk(&testUser{}, BulkOptions{Unordered: true}).
		InsertOne(bson.D{{Key: "email", Value: "u0@test.com"}, {Key: "name", Value: "dup"}}).
		InsertOne(bson.D{{Key: "email", Value: "ok@test.com"}, {Key: "name", Value: "ok"}}).
		Execute(ctx)
	var be *BulkError
	if !errors.As(err, &be) {
		t.Fatalf("expected *BulkError, got %v", err)
	}
	if len(be.Errors) != 1 || be.Errors[0].Index != 0 || !errors.Is(be.Errors[0].Err, ErrDuplicateKey) {
		t.Errorf("unexpected errors: %+v", be.Errors)
	}
	if !errors.Is(err, ErrDuplicateKey) {
		t.Error("BulkError should unwrap to its items' errors")
	}
	if res == nil || res.InsertedCount != 1 || be.Result.InsertedCount != 1 {
		t.Errorf("unordered bulk should have inserted the valid document: %+v", res)
	}
	if len(mw) == 0 || mw[len(mw)-1] != OpBulkWrite {
		t.Errorf("expected OpBulkWrite middleware, got %v", mw)
	}
}

func TestCreateMany_DuplicateKey_OrderedAndUnordered(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	seedUsers(t, ctx, 1) // u0@test.com exists

	batch := func() []*testUser {
		return []*testUser{
			{Email: "a@test.com", Name: "A"},
			{Email: "u0@test.com", Name: "Dup"},
			{Email: "b@test.com", Name: "B"},
		}
	}

	// Ordered: stops at the duplicate.
	err := CreateMany(ctx, batch())
	var be *BulkError
	if !errors.As(err, &be) || len(be.Errors) != 1 || be.Errors[0].Index != 1 {
		t.Fatalf("ordered: expected BulkError at index 1, got %v", err)
	}
	if be.Result.InsertedCount != 1 {
		t.Errorf("ordered: expected 1 inserted, got %d", be.Result.InsertedCount)
	}
	if ok, _ := Exists(ctx, bson.D{{Key: "email", Value: "b@test.com"}}, &testUser{}); ok {
		t.Error("ordered insert must not continue past the failure")
	}
	if _, err := DeleteMany(ctx, bson.D{{Key: "email", Value: "a@test.com"}}, &testUser{}); err != nil {
		t.Fatal(err)
	}

	// Unordered: inserts everything it can.
	err = CreateMany(ctx, batch(), CreateOptions{Unordered: true})
	if !errors.As(err, &be) || len(be.Errors) != 1 || be.Errors[0].Index != 1 {
		t.Fatalf("unordered: expected BulkError at index 1, got %v", err)
	}
	if be.Result.InsertedCount != 2 {
		t.Errorf("unordered: expected 2 inserted, got %d", be.Result.InsertedCount)
	}
	n, _ := Count(ctx, bson.D{}, &testUser{})
	if n != 3 {
		t.Errorf("expected 3 documents, got %d", n)
	}
}

func TestSaveMany_HappyPath(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	hooked := []*testHookUser{
		{Email: "h1@test.com", Name: "H1"},
		{Email: "h2@test.com", Name: "H2"},
	}
	if err := CreateMany(ctx, hooked); err != nil {
		t.Fatal(err)
	}
	for _, h := range hooked {
		h.Name += " saved"
	}
	if err := SaveMany(ctx, hooked); err != nil {
		t.Fatalf("SaveMany: %v", err)
	}
	for i, h := range hooked {
		if h.Version != 1 {
			t.Errorf("item %d: version %d, want 1", i, h.Version)
		}
		if got := h.Events[len(h.Events)-2:]; got[0] != "before_save" || got[1] != "after_save" {
			t.Errorf("item %d: hooks %v", i, h.Events)
		}
		var fresh testHookUser
		if err := FindByID(ctx, h.ID, &fresh); err != nil || fresh.Name != h.Name || fresh.Version != 1 {
			t.Errorf("item %d not persisted: %+v %v", i, fresh, err)
		}
	}

	// A plain value slice works too, and a second save bumps the version again.
	values := []testHookUser{*hooked[0], *hooked[1]}
	if err := SaveMany(ctx, values); err != nil {
		t.Fatalf("SaveMany values: %v", err)
	}
	if values[0].Version != 2 || values[1].Version != 2 {
		t.Errorf("versions after second save: %d %d", values[0].Version, values[1].Version)
	}
}

func TestSaveMany_ClassifiesConflictsAndMissing(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	users := seedUsers(t, ctx, 5)

	// Item 1: modified by another writer (stale version in our slice).
	other := *users[1]
	other.Age = 100
	if err := Update(ctx, &other); err != nil {
		t.Fatal(err)
	}
	// Item 2: deleted underneath us.
	if err := DeleteOne(ctx, bson.D{{Key: "_id", Value: users[2].ID}}, &testUser{}); err != nil {
		t.Fatal(err)
	}
	// Item 3: duplicate key against item 0.
	users[3].Email = users[0].Email

	for _, u := range users {
		u.Age += 1
	}
	err := SaveMany(ctx, users)
	var be *BulkError
	if !errors.As(err, &be) {
		t.Fatalf("expected *BulkError, got %v", err)
	}
	byIndex := map[int]error{}
	for _, e := range be.Errors {
		byIndex[e.Index] = e.Err
	}
	if len(byIndex) != 3 {
		t.Fatalf("expected 3 failures, got %+v", be.Errors)
	}
	if !errors.Is(byIndex[1], ErrVersionConflict) {
		t.Errorf("item 1 should be a version conflict, got %v", byIndex[1])
	}
	if !errors.Is(byIndex[2], ErrNotFound) {
		t.Errorf("item 2 should be not found, got %v", byIndex[2])
	}
	if !errors.Is(byIndex[3], ErrDuplicateKey) {
		t.Errorf("item 3 should be a duplicate key, got %v", byIndex[3])
	}
	if !errors.Is(err, ErrVersionConflict) || !errors.Is(err, ErrNotFound) {
		t.Error("BulkError should match each item error with errors.Is")
	}

	// Successful items were saved and advanced.
	for _, i := range []int{0, 4} {
		if users[i].Version != 1 {
			t.Errorf("item %d version %d, want 1", i, users[i].Version)
		}
		var fresh testUser
		if err := FindByID(ctx, users[i].ID, &fresh); err != nil || fresh.Age != users[i].Age {
			t.Errorf("item %d not saved: %+v %v", i, fresh, err)
		}
	}
	// The conflicting model's version was refreshed to the database's.
	if users[1].Version != 1 {
		t.Errorf("conflicting item should have version refreshed to 1, got %d", users[1].Version)
	}
	var fresh testUser
	if err := FindByID(ctx, users[1].ID, &fresh); err != nil || fresh.Age != 100 {
		t.Errorf("conflicting write must not overwrite the other writer: %+v", fresh)
	}
	// Failed non-conflict items were rolled back.
	if users[2].Version != 0 || users[3].Version != 0 {
		t.Errorf("failed items should roll back their version: %d %d", users[2].Version, users[3].Version)
	}
	if be.Result == nil || be.Result.MatchedCount != 2 {
		t.Errorf("result should reflect the two successful replacements: %+v", be.Result)
	}
}

func TestSaveMany_ImmutableViolationAbortsBeforeWrite(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	users := seedUsers(t, ctx, 2)

	users[0].Age = 50
	users[1].Name = "Renamed" // immutable
	err := SaveMany(ctx, users)
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationErrors, got %v", err)
	}
	var fresh testUser
	if err := FindByID(ctx, users[0].ID, &fresh); err != nil || fresh.Age == 50 {
		t.Errorf("nothing should be written when a model fails pre-write checks: %+v", fresh)
	}
	if users[0].Version != 0 {
		t.Errorf("versions must be untouched: %d", users[0].Version)
	}
}

func TestSaveMany_RejectsZeroIDAndEmptyIsNoop(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	if err := SaveMany(ctx, []*testUser{}); err != nil {
		t.Errorf("empty: %v", err)
	}
	err := SaveMany(ctx, []*testUser{{Email: "x@test.com", Name: "X"}})
	if err == nil {
		t.Error("zero ID must be rejected")
	}
}
