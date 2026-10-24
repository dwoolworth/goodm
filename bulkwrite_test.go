package goodm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
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
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "clientBulk", true: "perDocument"}[legacy], func(t *testing.T) {
			forceLegacySaveMany = legacy
			defer func() { forceLegacySaveMany = false }()
			testSaveManyClassification(t)
		})
	}
}

func testSaveManyClassification(t *testing.T) {
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

// testWC5 forces an unsatisfiable write concern on a single-node replica set.
type testWC5 struct {
	Model  `bson:",inline"`
	Name   string `bson:"name"`
	Events []string
}

func (m *testWC5) CollectionOptions() CollectionOptions {
	return CollectionOptions{WriteConcern: &writeconcern.WriteConcern{W: 5}}
}
func (m *testWC5) AfterCreate(ctx context.Context) error {
	m.Events = append(m.Events, "after_create")
	return nil
}
func (m *testWC5) AfterSave(ctx context.Context) error {
	m.Events = append(m.Events, "after_save")
	return nil
}

// testNoTimestamps is a custom-ID model with no timestamp or version fields.
type testNoTimestamps struct {
	ID   string `bson:"_id"`
	Name string `bson:"name"`
}

func TestSaveMany_ConcurrentWritersFromSameVersion_ExactlyOneWins(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "clientBulk", true: "perDocument"}[legacy], func(t *testing.T) {
			forceLegacySaveMany = legacy
			defer func() { forceLegacySaveMany = false }()
			testSaveManyRace(t)
		})
	}
}

func testSaveManyRace(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	for round := 0; round < 25; round++ {
		base := &testUser{Email: fmt.Sprintf("race%d@test.com", round), Name: "Race", Age: 1}
		if err := Create(ctx, base); err != nil {
			t.Fatal(err)
		}
		a, b := *base, *base
		a.Age = 10
		b.Role = "admin"

		var errA, errB error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); errA = SaveMany(ctx, []*testUser{&a}) }()
		go func() { defer wg.Done(); errB = SaveMany(ctx, []*testUser{&b}) }()
		wg.Wait()

		okA, okB := errA == nil, errB == nil
		if okA == okB {
			t.Fatalf("round %d: exactly one writer must win, got errA=%v errB=%v", round, errA, errB)
		}
		loser := errB
		if !okA {
			loser = errA
		}
		if !errors.Is(loser, ErrVersionConflict) {
			t.Fatalf("round %d: loser must see ErrVersionConflict, got %v", round, loser)
		}
		var fresh testUser
		if err := FindByID(ctx, base.ID, &fresh); err != nil {
			t.Fatal(err)
		}
		if fresh.Version != 1 {
			t.Fatalf("round %d: stored version %d, want 1", round, fresh.Version)
		}
		if okA && (fresh.Age != 10 || fresh.Role != "user") || okB && (fresh.Role != "admin" || fresh.Age != 1) {
			t.Fatalf("round %d: stored document does not match the winner: %+v (A won: %v)", round, fresh, okA)
		}
	}
}

func TestSaveMany_RejectsDuplicateIDsInBatch(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	u := seedUsers(t, ctx, 1)[0]

	copy1, copy2 := *u, *u
	copy1.Age = 1
	copy2.Age = 2
	err := SaveMany(ctx, []*testUser{&copy1, &copy2})
	if err == nil || !strings.Contains(err.Error(), "same _id") {
		t.Fatalf("expected duplicate-id rejection, got %v", err)
	}
	var fresh testUser
	if err := FindByID(ctx, u.ID, &fresh); err != nil || fresh.Version != 0 {
		t.Fatalf("nothing should have been written: %+v %v", fresh, err)
	}
}

func TestSaveMany_IdenticalContentFromAnotherWriter_IsStillAConflict(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	u := seedUsers(t, ctx, 1)[0]

	other := *u
	other.Age = 30
	if err := Update(ctx, &other); err != nil {
		t.Fatal(err)
	}
	mine := other
	mine.Version = 0 // stale: our filter targets version 0
	err := SaveMany(ctx, []*testUser{&mine})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("a stale version must be a conflict regardless of content, got %v", err)
	}
	if mine.Version != 1 {
		t.Errorf("conflicting model should have its version refreshed to 1, got %d", mine.Version)
	}
}

func TestSaveMany_ModelWithoutTimestamps_ClassifiesCorrectly(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testNoTimestamps{}, "test_no_ts", "")

	docs := []*testNoTimestamps{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}
	if err := CreateMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := DeleteOne(ctx, bson.D{{Key: "_id", Value: "b"}}, &testNoTimestamps{}); err != nil {
		t.Fatal(err)
	}
	docs[0].Name = "A2"
	docs[1].Name = "B2"
	err := SaveMany(ctx, docs)
	var be *BulkError
	if !errors.As(err, &be) || len(be.Errors) != 1 || be.Errors[0].Index != 1 || !errors.Is(be.Errors[0].Err, ErrNotFound) {
		t.Fatalf("expected only item 1 to fail with ErrNotFound, got %v", err)
	}
	var fresh testNoTimestamps
	if err := FindByID(ctx, "a", &fresh); err != nil || fresh.Name != "A2" {
		t.Fatalf("item 0 should have been saved and reported as success: %+v %v", fresh, err)
	}
}

func TestBulkPaths_WriteConcernErrorIsReported(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testWC5{}, "test_wc5", "")

	m := []*testWC5{{Name: "x"}, {Name: "y"}}
	err := CreateMany(ctx, m)
	var be *BulkError
	if !errors.As(err, &be) || be.WriteConcernError == nil {
		t.Fatalf("CreateMany: expected a BulkError carrying the write concern error, got %v", err)
	}
	for i, d := range m {
		if len(d.Events) != 0 {
			t.Errorf("CreateMany: AfterCreate must not run on item %d after a write concern failure", i)
		}
	}

	// The server may well have applied the inserts before failing the write
	// concern, so make sure the documents exist without assuming either way.
	coll := DB().Collection("test_wc5")
	for _, d := range m {
		if _, err := coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: d.ID}}, d, options.Replace().SetUpsert(true)); err != nil {
			t.Fatal(err)
		}
	}
	for _, legacy := range []bool{false, true} {
		forceLegacySaveMany = legacy
		m[0].Name = "x2"
		err = SaveMany(ctx, m)
		if !errors.As(err, &be) || be.WriteConcernError == nil {
			t.Fatalf("SaveMany (legacy=%v): expected a BulkError carrying the write concern error, got %v", legacy, err)
		}
		if m[0].Version != 0 || len(m[0].Events) != 0 {
			t.Errorf("SaveMany (legacy=%v): version must be rolled back and AfterSave skipped, got v=%d events=%v", legacy, m[0].Version, m[0].Events)
		}
	}
	forceLegacySaveMany = false

	_, err = NewBulk(&testWC5{}).InsertOne(bson.D{{Key: "name", Value: "z"}}).Execute(ctx)
	if !errors.As(err, &be) || be.WriteConcernError == nil {
		t.Fatalf("Bulk: expected a BulkError carrying the write concern error, got %v", err)
	}

	// A write concern error alongside a rejected item keeps the item error.
	id := bson.NewObjectID()
	dup := []*testWC5{{Model: Model{ID: id}, Name: "a"}, {Model: Model{ID: id}, Name: "b"}}
	err = CreateMany(ctx, dup, CreateOptions{Unordered: true})
	if !errors.As(err, &be) || be.WriteConcernError == nil || !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("expected both the write concern error and the duplicate key error, got %v", err)
	}
}

func TestSaveMany_MiddlewareScopeFilter(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	users := seedUsers(t, ctx, 2) // users[0] admin, users[1] user

	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		if op.Operation == OpSaveMany {
			op.Filter = bson.D{{Key: "role", Value: "admin"}}
		}
		return next(ctx)
	})
	for _, u := range users {
		u.Age = 99
	}
	err := SaveMany(ctx, users)
	var be *BulkError
	if !errors.As(err, &be) || len(be.Errors) != 1 || be.Errors[0].Index != 1 || !errors.Is(be.Errors[0].Err, ErrNotFound) {
		t.Fatalf("out-of-scope document must be reported as ErrNotFound, got %v", err)
	}
	var fresh testUser
	if err := FindByID(ctx, users[1].ID, &fresh); err != nil || fresh.Age == 99 {
		t.Fatalf("out-of-scope document must not be written: %+v %v", fresh, err)
	}
	if err := FindByID(ctx, users[0].ID, &fresh); err != nil || fresh.Age != 99 {
		t.Fatalf("in-scope document must be written: %+v %v", fresh, err)
	}
}
