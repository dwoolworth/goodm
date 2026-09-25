package goodm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func getIndexSpec(t *testing.T, ctx context.Context, coll *mongo.Collection, name string) (IndexSpec, bool) {
	t.Helper()
	specs, err := ListExistingIndexes(ctx, coll)
	if err != nil {
		t.Fatalf("ListExistingIndexes: %v", err)
	}
	spec, ok := specs[name]
	return spec, ok
}

func createNonUniqueIndex(t *testing.T, ctx context.Context, coll *mongo.Collection, field string) {
	t.Helper()
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: field, Value: 1}},
	})
	if err != nil {
		t.Fatalf("failed to create non-unique index: %v", err)
	}
}

func TestEnforce_CreatesUniqueIndex(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	if err := Enforce(ctx, db); err != nil {
		t.Fatalf("Enforce: %v", err)
	}

	spec, ok := getIndexSpec(t, ctx, db.Collection("test_users"), "email_1")
	if !ok {
		t.Fatal("expected email_1 index to be created")
	}
	if !spec.Unique {
		t.Error("expected email_1 index to be unique")
	}
}

func TestEnforce_NonUniqueExisting_ErrorsByDefault(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	coll := db.Collection("test_users")
	createNonUniqueIndex(t, ctx, coll, "email")

	err := Enforce(ctx, db)
	if err == nil {
		t.Fatal("expected EnforcementError for non-unique email_1, got nil")
	}
	var ee *EnforcementError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *EnforcementError, got %T: %v", err, err)
	}
	if !strings.Contains(ee.Message, "email_1") || !strings.Contains(ee.Message, "not unique") {
		t.Errorf("unexpected error message: %s", ee.Message)
	}

	// The mismatched index must not have been touched.
	spec, ok := getIndexSpec(t, ctx, coll, "email_1")
	if !ok {
		t.Fatal("email_1 index should still exist")
	}
	if spec.Unique {
		t.Error("email_1 should still be non-unique (error policy must not modify indexes)")
	}
}

func TestEnforce_NonUniqueExisting_Rebuild(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	coll := db.Collection("test_users")
	createNonUniqueIndex(t, ctx, coll, "email")

	err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchRebuild})
	if err != nil {
		t.Fatalf("Enforce with rebuild: %v", err)
	}

	spec, ok := getIndexSpec(t, ctx, coll, "email_1")
	if !ok {
		t.Fatal("email_1 index should exist after rebuild")
	}
	if !spec.Unique {
		t.Error("email_1 should be unique after rebuild")
	}
}

func TestEnforce_RebuildWithDuplicates_ErrorsWithoutDropping(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	coll := db.Collection("test_users")
	createNonUniqueIndex(t, ctx, coll, "email")

	// Insert duplicate emails directly, bypassing goodm validation.
	docs := []interface{}{
		bson.D{{Key: "email", Value: "dup@example.com"}, {Key: "name", Value: "a"}},
		bson.D{{Key: "email", Value: "dup@example.com"}, {Key: "name", Value: "b"}},
	}
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatalf("insert duplicates: %v", err)
	}

	err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchRebuild})
	if err == nil {
		t.Fatal("expected error rebuilding unique index over duplicate data")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-data error, got: %v", err)
	}

	// The existing index must survive: dropping it would leave no index at all.
	spec, ok := getIndexSpec(t, ctx, coll, "email_1")
	if !ok {
		t.Fatal("email_1 index must not be dropped when data has duplicates")
	}
	if spec.Unique {
		t.Error("email_1 should still be the original non-unique index")
	}
}

func TestEnforce_IgnorePolicy_KeepsLegacyBehavior(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	coll := db.Collection("test_users")
	createNonUniqueIndex(t, ctx, coll, "email")

	err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchIgnore})
	if err != nil {
		t.Fatalf("Enforce with ignore policy: %v", err)
	}

	spec, _ := getIndexSpec(t, ctx, coll, "email_1")
	if spec.Unique {
		t.Error("ignore policy must not modify the existing index")
	}
}

// testUniqueCompound has a unique compound index for mismatch testing.
type testUniqueCompound struct {
	Model `bson:",inline"`
	A     string `bson:"a"`
	B     string `bson:"b"`
}

func (m *testUniqueCompound) Indexes() []CompoundIndex {
	return []CompoundIndex{NewUniqueCompoundIndex("a", "b")}
}

func TestEnforce_CompoundUniqueMismatch_Rebuild(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	if err := Register(&testUniqueCompound{}, "test_unique_compound"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer func() {
		registryMu.Lock()
		delete(registry, "testUniqueCompound")
		registryMu.Unlock()
	}()

	coll := db.Collection("test_unique_compound")
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 1}},
	})
	if err != nil {
		t.Fatalf("create non-unique compound index: %v", err)
	}

	if err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchRebuild}); err != nil {
		t.Fatalf("Enforce: %v", err)
	}

	spec, ok := getIndexSpec(t, ctx, coll, "a_1_b_1")
	if !ok {
		t.Fatal("a_1_b_1 index should exist")
	}
	if !spec.Unique {
		t.Error("a_1_b_1 should be unique after rebuild")
	}
}

// testIndexedField has a plain (non-unique) indexed field.
type testIndexedField struct {
	Model `bson:",inline"`
	Code  string `bson:"code" goodm:"index"`
}

func TestEnforce_UniqueExistingForIndexField_Errors(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	if err := Register(&testIndexedField{}, "test_indexed_field"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer func() {
		registryMu.Lock()
		delete(registry, "testIndexedField")
		registryMu.Unlock()
	}()

	coll := db.Collection("test_indexed_field")
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "code", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		t.Fatalf("create unique index: %v", err)
	}

	err = Enforce(ctx, db)
	if err == nil {
		t.Fatal("expected error: existing index is unique but schema expects non-unique")
	}
	if !strings.Contains(err.Error(), "code_1") {
		t.Errorf("expected error to mention code_1, got: %v", err)
	}
}
