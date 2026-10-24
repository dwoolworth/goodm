package goodm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/session"
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
		`{Fields: []string{"body:text", "title:text"}, Name: "title_text_body_text"},`,
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

// --- second-round review findings ---

type testArrayField struct {
	Model  `bson:",inline"`
	Coords [2]float64 `bson:"coords"`
	Score  int        `bson:"score"`
	Nested struct {
		Inner []testOrderItem `bson:"inner"`
	} `bson:"nested"`
}

func TestUpdateFields_ConversionIsValidatedNotPanicking(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testArrayField{}, "test_array_field", "")

	m := &testArrayField{Coords: [2]float64{1, 2}, Score: 1}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}

	var ve ValidationErrors
	// short slice into an array: previously passed validation, wrote, then
	// panicked. It now converts to the padded array the field can hold, and
	// that is what is stored and reflected.
	if err := UpdateFields(ctx, m, bson.M{"coords": []float64{1}}); err != nil || m.Coords != [2]float64{1, 0} {
		t.Errorf("short slice for an array: err=%v coords=%v", err, m.Coords)
	}
	var stored testArrayField
	if err := FindByID(ctx, m.ID, &stored); err != nil || stored.Coords != [2]float64{1, 0} {
		t.Errorf("stored array should be the converted value: %v %v", stored.Coords, err)
	}
	// struct of a different type: previously panicked in validation
	if err := UpdateFields(ctx, m, bson.M{"nested": struct{ Inner int }{3}}); !errors.As(err, &ve) {
		t.Errorf("mismatched struct must be a validation error, got %v", err)
	}
	// a float that the int field cannot hold
	if err := UpdateFields(ctx, m, bson.M{"score": 5.7}); !errors.As(err, &ve) {
		t.Errorf("5.7 into an int must be a validation error, got %v", err)
	}
	// an integral float is fine and is reflected as an int
	if err := UpdateFields(ctx, m, bson.M{"score": 7.0}); err != nil || m.Score != 7 {
		t.Errorf("7.0 into an int: %v score=%d", err, m.Score)
	}
	// bson.D for a []struct field must not be accepted as elements
	if err := UpdateFields(ctx, m, bson.M{"nested.inner": bson.D{{Key: "name", Value: "x"}, {Key: "quantity", Value: 1}}}); !errors.As(err, &ve) {
		t.Errorf("bson.D for a slice field must be a validation error, got %v", err)
	}
	// a struct of another type with required fields missing
	if err := UpdateFields(ctx, m, bson.M{"nested.inner": []struct{}{{}}}); !errors.As(err, &ve) {
		t.Errorf("elements missing required fields must be rejected, got %v", err)
	}
	// array element path works
	if err := UpdateFields(ctx, m, bson.M{"coords.1": 9.5}); err != nil || m.Coords[1] != 9.5 {
		t.Errorf("array element: %v coords=%v", err, m.Coords)
	}
	if err := UpdateFields(ctx, m, bson.M{"coords.-1": 1.0}); err == nil {
		t.Error("negative index must be rejected")
	}
	// nil for a slice tagged min=1 fails like an empty slice
	o := &testOrder{Name: "o", Address: testAddress{Street: "s", City: "c"}, Items: []testOrderItem{{Name: "a", Quantity: 1}}}
	if err := Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFields(ctx, o, bson.M{"items.0.quantity": nil}); !errors.As(err, &ve) {
		t.Errorf("nil for a min=1 field must be a validation error, got %v", err)
	}
}

func TestUpdateFields_MapValuesAreReflectedOntoModel(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	o := &testOrder{Name: "o", Address: testAddress{Street: "1 St", City: "Old"}, Items: []testOrderItem{{Name: "w", Quantity: 1}}}
	if err := Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	err := UpdateFields(ctx, o, bson.M{
		"address": bson.M{"street": "2 St", "city": "New"},
		"items":   bson.A{bson.M{"name": "a", "quantity": 3}, bson.M{"name": "b", "quantity": 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Address.City != "New" || len(o.Items) != 2 || o.Items[1].Quantity != 4 {
		t.Fatalf("map values not reflected onto the model: %+v %+v", o.Address, o.Items)
	}
	// A following Update must not undo them.
	o.Name = "renamed"
	if err := Update(ctx, o); err != nil {
		t.Fatal(err)
	}
	var fresh testOrder
	if err := FindByID(ctx, o.ID, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Address.City != "New" || len(fresh.Items) != 2 || fresh.Name != "renamed" {
		t.Errorf("Update after UpdateFields lost the map-shaped changes: %+v", fresh)
	}
}

func TestRegister_RejectsModelEmbeddedWithoutInline(t *testing.T) {
	type noInline struct {
		Model
		Name string `bson:"name"`
	}
	err := Register(&noInline{}, "no_inline")
	if err == nil {
		Unregister(&noInline{})
		t.Fatal("embedding goodm.Model without bson:\",inline\" must be rejected")
	}
	if !strings.Contains(err.Error(), "inline") {
		t.Errorf("error should explain the inline tag: %v", err)
	}
}

func TestWithTransaction_CallbackErrorIsNotMistakenForNesting(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	sess, err := db.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(ctx)
	sessCtx := mongo.NewSessionContext(ctx, sess)

	calls := 0
	u := &testUser{Email: "nest@test.com", Name: "N"}
	err = WithTransaction(sessCtx, func(ctx context.Context) error {
		calls++
		if err := Create(ctx, u); err != nil {
			return err
		}
		// A callback that itself reports the driver's in-progress error must
		// not make goodm think it is nested and rerun it outside a transaction.
		return fmt.Errorf("wrapped: %w", session.ErrTransactInProgress)
	})
	if err == nil {
		t.Fatal("expected the callback error")
	}
	if calls != 1 {
		t.Errorf("callback ran %d times, want 1", calls)
	}
	if ok, _ := Exists(ctx, bson.D{{Key: "email", Value: "nest@test.com"}}, &testUser{}); ok {
		t.Error("the write must have been rolled back")
	}
}

func TestMiddleware_TypedNilScopeIsIgnored(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	u := seedUsers(t, ctx, 1)[0]

	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		if op.Operation == OpUpdate || op.Operation == OpDelete {
			var f bson.D
			op.Filter = f
		}
		return next(ctx)
	})
	u.Age = 5
	if err := Update(ctx, u); err != nil {
		t.Errorf("typed nil scope on Update: %v", err)
	}
	if err := UpdateFields(ctx, u, bson.M{"age": 6}); err != nil {
		t.Errorf("typed nil scope on UpdateFields: %v", err)
	}
	if err := Delete(ctx, u); err != nil {
		t.Errorf("typed nil scope on Delete: %v", err)
	}
}

type testSharedConflict struct {
	Model `bson:",inline"`
	B     string `bson:"b" goodm:"index"` // testSharedB declares b unique
	Kind  string `bson:"kind"`
}

func TestPlanMigration_ConflictingDefinitionsAreAnError(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testSharedB{}, "test_shared", "")
	registerModel(t, &testSharedConflict{}, "test_shared", "")

	sb, _ := SchemaOf(&testSharedB{})
	sc, _ := SchemaOf(&testSharedConflict{})
	if _, err := PlanMigration(ctx, db, []*Schema{sb, sc}); err == nil || !strings.Contains(err.Error(), "defined differently") {
		t.Fatalf("expected a conflict error, got %v", err)
	}
}

func TestDiscover_CustomNamesAndDottedPaths(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	coll := db.Collection("disc2")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "title", Value: "t"}, {Key: "addr", Value: bson.D{{Key: "city", Value: "c"}}}}); err != nil {
		t.Fatal(err)
	}
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "title", Value: 1}}, Options: options.Index().SetName("custom_title")},
		{Keys: bson.D{{Key: "addr.city", Value: 1}}},
		{Keys: bson.D{{Key: "missing", Value: -1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := Discover(ctx, db, DiscoverOptions{Collections: []string{"disc2"}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := GenerateModel(found[0], GenerateOptions{PackageName: "models", EmbedModel: true})
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, want := range []string{
		`{Fields: []string{"title"}, Name: "custom_title"},`,
		`goodm.NewCompoundIndex("addr.city"),`,
		`goodm.NewCompoundIndex("-missing"),`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("generated code missing %q:\n%s", want, code)
		}
	}
	if strings.Contains(code, `goodm:"index"`) {
		t.Errorf("a custom-named index must not become a plain tag:\n%s", code)
	}
}

// --- third-round review findings ---

func TestUpdateFields_RejectsPathsNotOnModel(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testArrayField{}, "test_array_field", "")

	o := &testOrder{Name: "o", Address: testAddress{Street: "s", City: "c"}, Items: []testOrderItem{{Name: "a", Quantity: 1}}}
	if err := Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	var ve ValidationErrors
	if err := UpdateFields(ctx, o, bson.M{"items.2": testOrderItem{Name: "z", Quantity: 5}}); !errors.As(err, &ve) {
		t.Errorf("index past the end of a slice must be rejected, got %v", err)
	}
	if err := UpdateFields(ctx, o, bson.M{"items.2.quantity": 5}); !errors.As(err, &ve) {
		t.Errorf("nested path past the end of a slice must be rejected, got %v", err)
	}
	m := &testArrayField{}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFields(ctx, m, bson.M{"coords.5": 1.0}); !errors.As(err, &ve) {
		t.Errorf("index past a fixed array must be rejected, got %v", err)
	}
	var fresh testOrder
	if err := FindByID(ctx, o.ID, &fresh); err != nil || len(fresh.Items) != 1 {
		t.Errorf("nothing should have been written: %+v %v", fresh.Items, err)
	}
}

func TestUpdateFields_ConversionWhitelist(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testArrayField{}, "test_array_field", "")

	u := seedUsers(t, ctx, 1)[0]
	m := &testArrayField{Score: 1}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	post := &testPost{Title: "p"}
	if err := Create(ctx, post); err != nil {
		t.Fatal(err)
	}

	var ve ValidationErrors
	rejected := []struct {
		model  interface{}
		fields bson.M
		why    string
	}{
		{m, bson.M{"score": true}, "bool into int"},
		{m, bson.M{"score": "7"}, "string into int"},
		{u, bson.M{"email": 42}, "number into string"},
		{post, bson.M{"author": "hello world!"}, "12-byte string into ObjectID"},
		{u, bson.M{"profile": int64(1)}, "number into ObjectID"},
		{m, bson.M{"coords": "nope"}, "string into array"},
		{m, bson.M{"nested": bson.A{1}}, "array into struct"},
		{m, bson.M{"nested.inner": []bson.M{{"name": "x", "quantity": true}}}, "bool into nested int"},
	}
	for _, c := range rejected {
		if err := UpdateFields(ctx, c.model, c.fields); !errors.As(err, &ve) {
			t.Errorf("%s should be a validation error, got %v", c.why, err)
		}
	}

	// Allowed conversions.
	hex := bson.NewObjectID()
	if err := UpdateFields(ctx, post, bson.M{"author": hex.Hex()}); err != nil || post.AuthorID != hex {
		t.Errorf("hex string into ObjectID: %v %v", err, post.AuthorID)
	}
	if err := UpdateFields(ctx, m, bson.M{"score": int64(9), "coords": []int{1, 2}}); err != nil || m.Score != 9 || m.Coords != [2]float64{1, 2} {
		t.Errorf("numeric widening and int array into float array: %v %+v", err, m)
	}
	if err := UpdateFields(ctx, m, bson.M{"score": uint8(3)}); err != nil || m.Score != 3 {
		t.Errorf("uint8 into int: %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"score": int64(math.MaxInt64)}); err != nil {
		t.Errorf("int64 into int on a 64-bit platform: %v", err)
	}
	// nil stores null and leaves the model at the zero value.
	if err := UpdateFields(ctx, u, bson.M{"age": nil}); err != nil || u.Age != 0 {
		t.Errorf("nil: %v age=%d", err, u.Age)
	}
	var raw bson.M
	if err := DB().Collection("test_users").FindOne(ctx, bson.D{{Key: "_id", Value: u.ID}}).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["age"]; !ok || v != nil {
		t.Errorf("nil should be stored as null, got %v", raw["age"])
	}
}

type testFloat32 struct {
	Model `bson:",inline"`
	F     float32 `bson:"f"`
}

func TestUpdateFields_Float32(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testFloat32{}, "test_f32", "")
	m := &testFloat32{}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFields(ctx, m, bson.M{"f": 0.1}); err != nil || m.F != float32(0.1) {
		t.Fatalf("0.1 into float32: %v f=%v", err, m.F)
	}
	var fresh testFloat32
	if err := FindByID(ctx, m.ID, &fresh); err != nil || fresh.F != float32(0.1) {
		t.Fatalf("stored float32 must read back: %v f=%v", err, fresh.F)
	}
}

// TestAudit is exported so the driver stores it (an unexported embedded
// struct is skipped by the driver and by goodm alike).
type TestAudit struct {
	By string `bson:"by"`
}

type testNonInlineEmbed struct {
	Model     `bson:",inline"`
	TestAudit        // not inline: stored as "testaudit": {by: ...}
	Name      string `bson:"name"`
}

func TestNonInlineEmbed_IsASubdocument(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testNonInlineEmbed{}, "test_non_inline", "")

	schema, _ := SchemaOf(&testNonInlineEmbed{})
	if schema.HasField("by") {
		t.Fatal("a non-inline embed must not be flattened into the schema")
	}
	if f := schema.GetField("testaudit"); f == nil || len(f.SubFields) != 1 {
		t.Fatalf("expected a testaudit subdocument field, got %+v", f)
	}

	m := &testNonInlineEmbed{TestAudit: TestAudit{By: "a"}, Name: "n"}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFields(ctx, m, bson.M{"by": "b"}); err == nil {
		t.Error("top-level \"by\" is not a field of this document and must be rejected")
	}
	if err := UpdateFields(ctx, m, bson.M{"testaudit.by": "b"}); err != nil || m.By != "b" {
		t.Fatalf("nested path through the embed: %v by=%q", err, m.By)
	}
	var fresh testNonInlineEmbed
	if err := FindByID(ctx, m.ID, &fresh); err != nil || fresh.By != "b" {
		t.Errorf("not persisted where the driver reads it: %+v %v", fresh, err)
	}
}

type testSharedPartialA struct {
	Model `bson:",inline"`
	Age   int `bson:"age"`
}

func (m *testSharedPartialA) Indexes() []CompoundIndex {
	return []CompoundIndex{{Fields: []string{"age"}, Name: "adults", PartialFilter: bson.M{"age": bson.M{"$gt": 5, "$lt": 100}}}}
}

type testSharedPartialB struct {
	Model `bson:",inline"`
	Age   int64 `bson:"age"`
}

func (m *testSharedPartialB) Indexes() []CompoundIndex {
	return []CompoundIndex{{Fields: []string{"age"}, Name: "adults", PartialFilter: bson.D{{Key: "age", Value: bson.D{{Key: "$lt", Value: int64(100)}, {Key: "$gt", Value: 5}}}}}}
}

func TestPlanMigration_EquivalentDefinitionsAreNotConflicts(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testSharedPartialA{}, "test_shared_partial", "")
	registerModel(t, &testSharedPartialB{}, "test_shared_partial", "")
	sa, _ := SchemaOf(&testSharedPartialA{})
	sb, _ := SchemaOf(&testSharedPartialB{})
	for i := 0; i < 50; i++ {
		if _, err := PlanMigration(ctx, db, []*Schema{sa, sb}); err != nil {
			t.Fatalf("run %d: equivalent partial filters reported as a conflict: %v", i, err)
		}
	}
	if !collationsEquivalent(&options.Collation{Locale: "en"}, &options.Collation{Locale: "en", Strength: 3}) {
		t.Error("collation defaults must compare equal")
	}
	if collationsEquivalent(&options.Collation{Locale: "en"}, &options.Collation{Locale: "en", Strength: 2}) {
		t.Error("different strengths must differ")
	}
	// Enforce twice with a nested-map partial filter must be stable too.
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchError}); err != nil {
			t.Fatalf("Enforce run %d reported a mismatch on an identical partial filter: %v", i, err)
		}
	}
}

func TestDiscover_TwoIndexesOnOneField(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	coll := db.Collection("disc3")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "email", Value: "e"}, {Key: "score", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "email", Value: "hashed"}}},
		{Keys: bson.D{{Key: "score", Value: 1}}, Options: options.Index().SetSparse(true)},
		{Keys: bson.D{{Key: "score", Value: -1}}, Options: options.Index().SetExpireAfterSeconds(60)},
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := Discover(ctx, db, DiscoverOptions{Collections: []string{"disc3"}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := GenerateModel(found[0], GenerateOptions{PackageName: "models", EmbedModel: true})
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	if strings.Contains(code, "unique,index=hashed") || strings.Contains(code, "index=-1,ttl=60\"") && strings.Contains(code, "sparse") && !strings.Contains(code, "Indexes()") {
		t.Errorf("indexes on one field must not be merged into one tag:\n%s", code)
	}
	tags := strings.Count(code, `goodm:"`)
	entries := strings.Count(code, "Fields: []string") + strings.Count(code, "goodm.NewCompoundIndex(") + strings.Count(code, "goodm.NewUniqueCompoundIndex(")
	if tags != 2 || entries != 2 {
		t.Errorf("expected 2 field tags and 2 Indexes() entries, got %d and %d:\n%s", tags, entries, code)
	}
}

// --- fourth-round review findings ---

// TestBase is exported: the driver skips a pointer to an unexported embed.
type TestBase struct {
	Note string `bson:"note"`
}

type testSubWithPtrEmbed struct {
	*TestBase `bson:",inline"`
	B         int `bson:"b"`
}

type testConvModel struct {
	Model   `bson:",inline"`
	Count   int64                `bson:"count"`
	Big     uint64               `bson:"big"`
	Ratio   float64              `bson:"ratio"`
	When    time.Time            `bson:"when"`
	Blob    []byte               `bson:"blob"`
	Doc     bson.D               `bson:"doc"`
	Sub     testSubWithPtrEmbed  `bson:"sub"`
	Ignored string               `bson:"-"`
	Meta    map[string]time.Time `bson:"meta"`
}

type testDTO struct {
	When time.Time `bson:"when"`
	Blob []byte    `bson:"blob"`
	Big  uint64    `bson:"big"`
}

func TestUpdateFields_ConverterEdgeCases(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testConvModel{}, "test_conv", "")
	m := &testConvModel{Sub: testSubWithPtrEmbed{TestBase: &TestBase{Note: "n"}}}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}

	// Pointer chains ending in nil must not panic and must store null.
	var nilInt *int64
	if err := UpdateFields(ctx, m, bson.M{"count": &nilInt}); err != nil || m.Count != 0 {
		t.Errorf("nil pointer chain: %v count=%d", err, m.Count)
	}
	var nilIface interface{}
	if err := UpdateFields(ctx, m, bson.M{"count": &nilIface}); err != nil {
		t.Errorf("pointer to nil interface: %v", err)
	}

	// A subdocument type with a nil inline pointer embed: fields through the
	// embed must be set, not dropped, and validation must not panic.
	if err := UpdateFields(ctx, m, bson.M{"sub": bson.M{"note": "new", "b": 2}}); err != nil {
		t.Fatalf("inline pointer embed in subdocument: %v", err)
	}
	if m.Sub.TestBase == nil || m.Sub.Note != "new" || m.Sub.B != 2 {
		t.Errorf("inline embed fields not set: %+v", m.Sub)
	}
	var fresh testConvModel
	if err := FindByID(ctx, m.ID, &fresh); err != nil || fresh.Sub.TestBase == nil || fresh.Sub.Note != "new" {
		t.Errorf("inline embed not persisted: %+v %v", fresh.Sub, err)
	}

	var ve ValidationErrors
	// Float boundaries into integer fields.
	if err := UpdateFields(ctx, m, bson.M{"count": 9.223372036854775807e18}); !errors.As(err, &ve) {
		t.Errorf("2^63 into int64 must be rejected, got %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"big": 1.8446744073709551615e19}); !errors.As(err, &ve) {
		t.Errorf("2^64 into uint64 must be rejected, got %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"count": -9.223372036854775808e18}); err != nil || m.Count != math.MinInt64 {
		t.Errorf("-2^63 into int64 is exact and must be accepted: %v", err)
	}
	// Lossy int into float is rejected; exact is accepted.
	if err := UpdateFields(ctx, m, bson.M{"ratio": int64(1<<53 + 1)}); !errors.As(err, &ve) {
		t.Errorf("2^53+1 into float64 must be rejected, got %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"ratio": int64(1 << 53)}); err != nil || m.Ratio != 1<<53 {
		t.Errorf("2^53 into float64: %v", err)
	}

	// Struct sources keep time.Time and []byte; bson.DateTime converts to time.
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := UpdateFields(ctx, m, bson.M{"when": bson.NewDateTimeFromTime(when)}); err != nil || !m.When.Equal(when) {
		t.Errorf("bson.DateTime into time.Time: %v %v", err, m.When)
	}
	if err := UpdateFields(ctx, m, bson.M{"count": bson.NewDateTimeFromTime(when)}); !errors.As(err, &ve) {
		t.Errorf("bson.DateTime into int64 must be rejected, got %v", err)
	}
	type dtoModel struct {
		Model `bson:",inline"`
		Sub   testDTO `bson:"sub"`
	}
	registerModel(t, &dtoModel{}, "test_dto", "")
	d := &dtoModel{}
	if err := Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	type otherDTO struct {
		When time.Time `bson:"when"`
		Blob []byte    `bson:"blob"`
		Big  uint64    `bson:"big"`
	}
	if err := UpdateFields(ctx, d, bson.M{"sub": otherDTO{When: when, Blob: []byte("x"), Big: 1 << 40}}); err != nil {
		t.Fatalf("struct of another type with time, bytes, and a large uint64: %v", err)
	}
	if !d.Sub.When.Equal(when) || string(d.Sub.Blob) != "x" || d.Sub.Big != 1<<40 {
		t.Errorf("struct source values lost: %+v", d.Sub)
	}
	if err := UpdateFields(ctx, m, bson.M{"blob": bson.Binary{Subtype: 0, Data: []byte("b")}}); err != nil || string(m.Blob) != "b" {
		t.Errorf("generic bson.Binary into []byte: %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"blob": bson.Binary{Subtype: 4, Data: []byte("b")}}); !errors.As(err, &ve) {
		t.Errorf("UUID binary into []byte must be rejected, got %v", err)
	}

	// bson.D field accepts any document source; "-" fields are never matched.
	if err := UpdateFields(ctx, m, bson.M{"doc": bson.M{"k": 1}}); err != nil || len(m.Doc) != 1 || m.Doc[0].Key != "k" {
		t.Errorf("bson.M into bson.D field: %v %v", err, m.Doc)
	}
	if err := UpdateFields(ctx, m, bson.M{"-": "x"}); err == nil {
		t.Error("a field tagged - must not be addressable")
	}
	// map of times via a document source
	if err := UpdateFields(ctx, m, bson.M{"meta": bson.M{"a": when}}); err != nil || !m.Meta["a"].Equal(when) {
		t.Errorf("map target: %v %v", err, m.Meta)
	}
}

type testNilEmbedModel struct {
	Model     `bson:",inline"`
	*TestBase `bson:",inline"`
	Name      string `bson:"name" goodm:"required"`
}

func TestCreate_NilInlinePointerEmbedDoesNotPanic(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testNilEmbedModel{}, "test_nil_embed", "")
	m := &testNilEmbedModel{Name: "n"}
	if err := Create(ctx, m); err != nil {
		t.Fatalf("Create with a nil inline pointer embed: %v", err)
	}
	m.Name = "n2"
	if err := Update(ctx, m); err != nil {
		t.Fatalf("Update with a nil inline pointer embed: %v", err)
	}
}

func TestCanonicalBSON_ExactIntegersAndSubdocumentOrder(t *testing.T) {
	a := bson.D{{Key: "x", Value: bson.D{{Key: "$gt", Value: int64(1<<53 + 1)}}}}
	b := bson.D{{Key: "x", Value: bson.D{{Key: "$gt", Value: int64(1 << 53)}}}}
	if filtersEquivalent(a, b) {
		t.Error("large int64 filters that differ must not compare equal")
	}
	c := bson.D{{Key: "x", Value: bson.D{{Key: "$gt", Value: float64(1 << 53)}}}}
	if filtersEquivalent(b, c) {
		t.Error("int64 2^53 and float64 2^53 differ in type beyond the exact range and must not compare equal")
	}
	if !filtersEquivalent(bson.M{"n": 5}, bson.D{{Key: "n", Value: 5.0}}) {
		t.Error("5 and 5.0 must compare equal")
	}
	// Operator documents may be reordered; exact-match subdocuments may not.
	if !filtersEquivalent(bson.D{{Key: "a", Value: bson.D{{Key: "$gt", Value: 1}, {Key: "$lt", Value: 5}}}}, bson.M{"a": bson.M{"$lt": 5, "$gt": 1}}) {
		t.Error("operator document order must not matter")
	}
	if filtersEquivalent(bson.D{{Key: "a", Value: bson.D{{Key: "x", Value: 1}, {Key: "y", Value: 2}}}}, bson.D{{Key: "a", Value: bson.D{{Key: "y", Value: 2}, {Key: "x", Value: 1}}}}) {
		t.Error("exact-match subdocument order must matter")
	}
}

func TestDiscover_EmbedModel_KeepsIndexesOnModelFields(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	coll := db.Collection("disc4")
	if _, err := coll.InsertOne(ctx, bson.D{{Key: "name", Value: "n"}, {Key: "created_at", Value: fixedTime}, {Key: "updated_at", Value: fixedTime}}); err != nil {
		t.Fatal(err)
	}
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "created_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(3600)}); err != nil {
		t.Fatal(err)
	}
	found, err := Discover(ctx, db, DiscoverOptions{Collections: []string{"disc4"}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := GenerateModel(found[0], GenerateOptions{PackageName: "models", EmbedModel: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `{Fields: []string{"created_at"}, ExpireAfterSeconds: goodm.TTL(3600 * time.Second)},`) {
		t.Errorf("TTL index on created_at must survive EmbedModel as an Indexes() entry:\n%s", src)
	}
}
