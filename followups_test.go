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

// --- UpdateFields with non-struct subdocument values (previously panicked) ---

func TestUpdateFields_SubdocumentValuesDoNotPanic(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	order := &testOrder{Name: "o", Address: testAddress{Street: "1 St", City: "Old"}, Items: []testOrderItem{{Name: "w", Quantity: 1}}}
	if err := Create(ctx, order); err != nil {
		t.Fatal(err)
	}

	var ve ValidationErrors
	// bson.M subdocument missing a required field
	err := UpdateFields(ctx, order, bson.M{"address": bson.M{"street": "S"}})
	if !errors.As(err, &ve) || !hasFieldError(ve, "address.city") {
		t.Errorf("bson.M subdocument should be validated by bson name, got %v", err)
	}
	// slice of bson.M with a min violation
	err = UpdateFields(ctx, order, bson.M{"items": []bson.M{{"name": "x", "quantity": 0}}})
	if !errors.As(err, &ve) || !hasFieldError(ve, "items[0].quantity") {
		t.Errorf("[]bson.M elements should be validated, got %v", err)
	}
	// bson.A of bson.D
	err = UpdateFields(ctx, order, bson.M{"items": bson.A{bson.D{{Key: "name", Value: ""}, {Key: "quantity", Value: 2}}}})
	if !errors.As(err, &ve) || !hasFieldError(ve, "items[0].name") {
		t.Errorf("bson.A/bson.D elements should be validated, got %v", err)
	}
	// wrong kinds are errors, not panics
	if err := UpdateFields(ctx, order, bson.M{"address": "nope"}); !errors.As(err, &ve) {
		t.Errorf("scalar for a subdocument should be a validation error, got %v", err)
	}
	if err := UpdateFields(ctx, order, bson.M{"items": "nope"}); !errors.As(err, &ve) {
		t.Errorf("scalar for a slice should be a validation error, got %v", err)
	}

	// Valid values of every shape are accepted and persisted.
	err = UpdateFields(ctx, order, bson.M{
		"address": bson.D{{Key: "street", Value: "2 St"}, {Key: "city", Value: "New"}},
		"items.0": testOrderItem{Name: "y", Quantity: 2},
	})
	if err != nil {
		t.Fatalf("valid subdocument values rejected: %v", err)
	}
	if order.Items[0].Quantity != 2 || order.Items[0].Name != "y" {
		t.Errorf("items.0 struct not reflected onto model: %+v", order.Items)
	}
	var fresh testOrder
	if err := FindByID(ctx, order.ID, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Address.City != "New" || fresh.Address.Street != "2 St" || fresh.Items[0].Quantity != 2 {
		t.Errorf("not persisted: %+v", fresh)
	}
	err = UpdateFields(ctx, order, bson.M{"items": []bson.M{{"name": "a", "quantity": 3}, {"name": "b", "quantity": 4}}})
	if err != nil {
		t.Fatalf("[]bson.M rejected: %v", err)
	}
	if err := FindByID(ctx, order.ID, &fresh); err != nil || len(fresh.Items) != 2 || fresh.Items[1].Quantity != 4 {
		t.Errorf("[]bson.M not persisted: %+v %v", fresh.Items, err)
	}
	// Element of a scalar slice path resolves and is not over-validated.
	post := &testPost{Title: "p", TagIDs: []bson.ObjectID{bson.NewObjectID()}}
	if err := Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFields(ctx, post, bson.M{"tags.0": bson.NewObjectID()}); err != nil {
		t.Errorf("scalar slice element: %v", err)
	}
}

// --- middleware scoping of Update, UpdateFields, Delete ---

func TestMiddleware_ScopesUpdateAndDeleteByID(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	users := seedUsers(t, ctx, 2) // users[0] admin, users[1] user

	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		if op.Operation == OpUpdate || op.Operation == OpDelete {
			op.Filter = bson.D{{Key: "role", Value: "admin"}}
		}
		return next(ctx)
	})

	users[1].Age = 99
	if err := Update(ctx, users[1]); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update of an out-of-scope document must be ErrNotFound, got %v", err)
	}
	if users[1].Version != 0 {
		t.Errorf("version must be rolled back, got %d", users[1].Version)
	}
	if err := UpdateFields(ctx, users[1], bson.M{"age": 99}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateFields of an out-of-scope document must be ErrNotFound, got %v", err)
	}
	if err := Delete(ctx, users[1]); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete of an out-of-scope document must be ErrNotFound, got %v", err)
	}
	var fresh testUser
	if err := FindByID(ctx, users[1].ID, &fresh); err != nil || fresh.Age == 99 {
		t.Errorf("out-of-scope document must be untouched: %+v %v", fresh, err)
	}

	users[0].Age = 99
	if err := Update(ctx, users[0]); err != nil {
		t.Errorf("in-scope Update: %v", err)
	}
	if err := UpdateFields(ctx, users[0], bson.M{"age": 100}); err != nil {
		t.Errorf("in-scope UpdateFields: %v", err)
	}
	if err := Update(ctx, users[0], WithRetry(2)); err != nil {
		t.Errorf("in-scope Update with retry after UpdateFields bumped the version: %v", err)
	}
	if err := Delete(ctx, users[0]); err != nil {
		t.Errorf("in-scope Delete: %v", err)
	}
}

// --- compound text indexes ---

type testTextIndexes struct {
	Model `bson:",inline"`
	Title string `bson:"title"`
	Body  string `bson:"body"`
}

func (m *testTextIndexes) Indexes() []CompoundIndex {
	return []CompoundIndex{NewCompoundIndex("title:text", "body:text")}
}

func TestEnforce_CompoundTextIndex_IsStable(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testTextIndexes{}, "test_text_indexes", "")

	if err := Enforce(ctx, db); err != nil {
		t.Fatalf("first Enforce: %v", err)
	}
	if err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchError}); err != nil {
		t.Fatalf("second Enforce reported a mismatch on a compound text index: %v", err)
	}
	schema, _ := SchemaOf(&testTextIndexes{})
	plan, err := PlanMigration(ctx, db, []*Schema{schema})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Type != ActionFieldDrift {
			t.Errorf("migrate should plan nothing for a matching text index, got %s", a.Description)
		}
	}
}

func TestIndexKeysEqual_TextFieldsAreASet(t *testing.T) {
	a := bson.D{{Key: "title", Value: "text"}, {Key: "body", Value: "text"}}
	b := bson.D{{Key: "body", Value: "text"}, {Key: "title", Value: "text"}}
	if !indexKeysEqual(a, b) {
		t.Error("text fields in different order must compare equal")
	}
	c := bson.D{{Key: "tenant", Value: int32(1)}, {Key: "title", Value: "text"}, {Key: "body", Value: "text"}}
	d := bson.D{{Key: "tenant", Value: int32(1)}, {Key: "body", Value: "text"}, {Key: "title", Value: "text"}}
	if !indexKeysEqual(c, d) {
		t.Error("text run after a regular key must compare as a set")
	}
	e := bson.D{{Key: "title", Value: "text"}, {Key: "tenant", Value: int32(1)}}
	if indexKeysEqual(c, e) {
		t.Error("different regular keys must not compare equal")
	}
}

// --- untagged ID is rejected ---

func TestRegister_RejectsModelWithoutIDTag(t *testing.T) {
	type noTag struct {
		ID   string
		Name string `bson:"name"`
	}
	err := Register(&noTag{}, "no_tag")
	if err == nil {
		Unregister(&noTag{})
		t.Fatal("a model whose ID field is not tagged _id must be rejected at Register")
	}
	if !strings.Contains(err.Error(), "_id") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- transaction detection ---

func TestWithTransaction_SessionWithoutTransaction_StartsOne(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	sess, err := db.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(ctx)
	sessCtx := mongo.NewSessionContext(ctx, sess)

	u := &testUser{Email: "txn@test.com", Name: "T"}
	err = WithTransaction(sessCtx, func(ctx context.Context) error {
		if err := Create(ctx, u); err != nil {
			return err
		}
		return errors.New("abort")
	})
	if err == nil {
		t.Fatal("expected the callback error")
	}
	if ok, _ := Exists(ctx, bson.D{{Key: "_id", Value: u.ID}}, &testUser{}); ok {
		t.Fatal("a session without a transaction must get one started: the write should have been rolled back")
	}
	// The session is reusable afterwards.
	if err := WithTransaction(sessCtx, func(ctx context.Context) error { return Create(ctx, u) }); err != nil {
		t.Fatalf("second transaction on the same session: %v", err)
	}
	if ok, _ := Exists(ctx, bson.D{{Key: "_id", Value: u.ID}}, &testUser{}); !ok {
		t.Fatal("committed write missing")
	}
}

// --- migrate with models sharing a collection ---

type testSharedA struct {
	Model `bson:",inline"`
	A     string `bson:"a" goodm:"index"`
	Kind  string `bson:"kind"`
}

type testSharedB struct {
	Model `bson:",inline"`
	B     string `bson:"b" goodm:"unique"`
	Kind  string `bson:"kind"`
}

func TestPlanMigration_ModelsSharingACollection(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testSharedA{}, "test_shared", "")
	registerModel(t, &testSharedB{}, "test_shared", "")

	sa, _ := SchemaOf(&testSharedA{})
	sb, _ := SchemaOf(&testSharedB{})
	plan, err := PlanMigration(ctx, db, []*Schema{sa, sb})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ExecuteMigration(ctx, db, plan, MigrateOptions{})
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("execute: %v %v", err, res.Errors)
	}

	plan, err = PlanMigration(ctx, db, []*Schema{sa, sb})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Type == ActionDropIndex || a.Type == ActionCreateIndex || a.Type == ActionRebuildIndex {
			t.Errorf("models sharing a collection must not plan each other's indexes as extras: %s", a.Description)
		}
	}
}

// --- discover fidelity ---

func TestDiscover_IndexKindsAndOptions(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	coll := db.Collection("disc")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "title", Value: "t"}, {Key: "body", Value: "b"}, {Key: "score", Value: 1}, {Key: "exp", Value: fixedTime}, {Key: "code", Value: "c"}, {Key: "email", Value: "e"}}); err != nil {
		t.Fatal(err)
	}
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "title", Value: "text"}, {Key: "body", Value: "text"}}},
		{Keys: bson.D{{Key: "score", Value: -1}}},
		{Keys: bson.D{{Key: "exp", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(3600)},
		{Keys: bson.D{{Key: "code", Value: 1}}, Options: options.Index().SetUnique(true).SetSparse(true)},
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.D{{Key: "active", Value: true}})},
	})
	if err != nil {
		t.Fatal(err)
	}

	found, err := Discover(ctx, db, DiscoverOptions{Collections: []string{"disc"}})
	if err != nil {
		t.Fatal(err)
	}
	dc := found[0]
	fields := map[string]DiscoveredField{}
	for _, f := range dc.Fields {
		fields[f.BSONName] = f
	}
	if f := fields["score"]; !f.IsIndexed || f.IndexKind != "-1" {
		t.Errorf("score: %+v", f)
	}
	if f := fields["exp"]; !f.IsIndexed || f.TTL == nil || *f.TTL != 3600 {
		t.Errorf("exp: %+v", f)
	}
	if f := fields["code"]; !f.IsUnique || !f.Sparse {
		t.Errorf("code: %+v", f)
	}
	if f := fields["email"]; f.IsUnique || f.IsIndexed {
		t.Errorf("email has a partial filter and must not become a plain tag: %+v", f)
	}

	src, err := GenerateModel(dc, GenerateOptions{PackageName: "models", EmbedModel: true})
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, want := range []string{
		`goodm:"index=-1,required"`,
		`goodm:"index,ttl=3600,required"`,
		`goodm:"unique,sparse,required"`,
		`goodm.NewCompoundIndex("body:text", "title:text")`,
		`{Fields: []string{"email"}, Unique: true},`,
		`TODO: index "email_1" has a partial filter`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
	if strings.Contains(code, "_fts") {
		t.Errorf("text index must not leak internal keys:\n%s", code)
	}
}
