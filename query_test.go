package goodm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func seedUsers(t *testing.T, ctx context.Context, n int) []*testUser {
	t.Helper()
	users := make([]*testUser, n)
	for i := range users {
		role := "user"
		if i%2 == 0 {
			role = "admin"
		}
		users[i] = &testUser{Email: fmt.Sprintf("u%d@test.com", i), Name: fmt.Sprintf("User %d", i), Age: 20 + i, Role: role}
		if err := Create(ctx, users[i]); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return users
}

// --- duplicate keys ---

func TestCreate_DuplicateKey_IsTyped(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}

	if err := Create(ctx, &testUser{Email: "dup@test.com", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	err := Create(ctx, &testUser{Email: "dup@test.com", Name: "B"})
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("expected *DuplicateKeyError, got %T", err)
	}
	if dke.Collection != "test_users" || dke.Index != "email_1" || len(dke.Fields) != 1 || dke.Fields[0] != "email" {
		t.Errorf("unexpected DuplicateKeyError details: %+v", dke)
	}
}

func TestUpdate_DuplicateKey_IsTyped(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	users := seedUsers(t, ctx, 2)
	users[1].Email = users[0].Email
	err := Update(ctx, users[1])
	if !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey from Update, got %v", err)
	}
	if users[1].Version != 0 {
		t.Errorf("version should be rolled back after failed replace, got %d", users[1].Version)
	}
}

// --- min/max semantics ---

func TestValidate_MinRejectsZero_AndFloatsCompareExactly(t *testing.T) {
	type scored struct {
		Qty   int     `bson:"qty"   goodm:"min=1"`
		Score float64 `bson:"score" goodm:"min=0,max=10"`
		Opt   *int    `bson:"opt"   goodm:"min=5"`
	}
	schema := &Schema{Fields: parseFields(modelType(&scored{}), nil)}

	errs := Validate(&scored{Qty: 0, Score: -0.5}, schema)
	if !hasFieldError(errs, "qty") {
		t.Errorf("min=1 must reject 0: %v", errs)
	}
	if !hasFieldError(errs, "score") {
		t.Errorf("min=0 must reject -0.5: %v", errs)
	}
	if hasFieldError(errs, "opt") {
		t.Errorf("nil pointer is absent and must not be checked: %v", errs)
	}

	errs = Validate(&scored{Qty: 1, Score: 10.9}, schema)
	if !hasFieldError(errs, "score") {
		t.Errorf("max=10 must reject 10.9: %v", errs)
	}
	three := 3
	errs = Validate(&scored{Qty: 1, Score: 10, Opt: &three}, schema)
	if !hasFieldError(errs, "opt") {
		t.Errorf("min=5 must reject *int pointing to 3: %v", errs)
	}
	if hasFieldError(errs, "score") {
		t.Errorf("max=10 must accept 10: %v", errs)
	}
}

func hasFieldError(errs []ValidationError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

// --- middleware filter modification ---

func TestMiddleware_CanReplaceFilterAndPipeline(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	seedUsers(t, ctx, 4) // 2 admins, 2 users

	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		switch op.Operation {
		case OpFind, OpDeleteMany:
			op.Filter = bson.D{{Key: "role", Value: "admin"}}
		case OpAggregate:
			op.Pipeline = append([]bson.D{{{Key: "$match", Value: bson.D{{Key: "role", Value: "admin"}}}}}, op.Pipeline...)
		}
		return next(ctx)
	})

	var users []testUser
	if err := Find(ctx, bson.D{}, &users); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("middleware filter ignored by Find: got %d users, want 2", len(users))
	}
	n, err := Count(ctx, bson.D{}, &testUser{})
	if err != nil || n != 2 {
		t.Fatalf("middleware filter ignored by Count: n=%d err=%v", n, err)
	}
	var one testUser
	if err := FindOne(ctx, bson.D{{Key: "role", Value: "user"}}, &one); err != nil || one.Role != "admin" {
		t.Fatalf("middleware filter ignored by FindOne: role=%q err=%v", one.Role, err)
	}

	var rows []bson.M
	if err := NewPipeline(&testUser{}).Count("n").Execute(ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || toInt64(rows[0]["n"]) != 2 {
		t.Fatalf("middleware pipeline ignored by aggregate: %v", rows)
	}

	res, err := DeleteMany(ctx, bson.D{}, &testUser{})
	if err != nil || res.DeletedCount != 2 {
		t.Fatalf("middleware filter ignored by DeleteMany: %+v err=%v", res, err)
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

// --- UpdateFields ---

func TestUpdateFields_ValidatesAndSupportsDottedPaths(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	u := seedUsers(t, ctx, 1)[0]

	var ve ValidationErrors
	if err := UpdateFields(ctx, u, bson.M{"role": "bogus"}); !errors.As(err, &ve) {
		t.Errorf("enum violation should be a ValidationErrors, got %v", err)
	}
	if err := UpdateFields(ctx, u, bson.M{"age": -1}); !errors.As(err, &ve) {
		t.Errorf("min violation should be a ValidationErrors, got %v", err)
	}
	if err := UpdateFields(ctx, u, bson.M{"name": "Other"}); !errors.As(err, &ve) {
		t.Errorf("immutable field should be rejected, got %v", err)
	}
	if err := UpdateFields(ctx, u, bson.M{"email": ""}); !errors.As(err, &ve) {
		t.Errorf("required field set to zero should be rejected, got %v", err)
	}

	fields := bson.M{"age": 42}
	if err := UpdateFields(ctx, u, fields); err != nil {
		t.Fatal(err)
	}
	if _, polluted := fields["updated_at"]; polluted || len(fields) != 1 {
		t.Errorf("caller's map was modified: %v", fields)
	}
	if u.Age != 42 {
		t.Errorf("value not reflected onto model: %d", u.Age)
	}

	order := &testOrder{Name: "o", Address: testAddress{Street: "1 St", City: "Old"}, Items: []testOrderItem{{Name: "w", Quantity: 1}}}
	if err := Create(ctx, order); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFields(ctx, order, bson.M{"address.city": "New", "items.0.quantity": 7}); err != nil {
		t.Fatalf("dotted paths: %v", err)
	}
	if order.Address.City != "New" || order.Items[0].Quantity != 7 {
		t.Errorf("dotted values not reflected: %+v", order)
	}
	var fresh testOrder
	if err := FindByID(ctx, order.ID, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Address.City != "New" || fresh.Items[0].Quantity != 7 || fresh.Version != 1 {
		t.Errorf("dotted values not persisted: %+v", fresh)
	}
	if err := UpdateFields(ctx, order, bson.M{"items.0.quantity": 0}); !errors.As(err, &ve) {
		t.Errorf("nested min violation should be rejected, got %v", err)
	}
	if err := UpdateFields(ctx, order, bson.M{"address.nope": 1}); err == nil {
		t.Error("unknown nested field should be rejected")
	}
}

// --- registry ---

func TestRegistry_SameNameDifferentTypes(t *testing.T) {
	type Dup struct {
		Model `bson:",inline"`
		A     string `bson:"a"`
	}
	first := &Dup{}
	var second interface{}
	func() {
		type Dup struct {
			Model `bson:",inline"`
			B     string `bson:"b"`
		}
		second = &Dup{}
	}()

	if err := Register(first, "dup_one"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(first)
	if err := Register(second, "dup_two"); err != nil {
		t.Fatalf("two types with the same name must both register: %v", err)
	}
	defer Unregister(second)
	if err := Register(first, "dup_three"); err == nil {
		t.Error("registering the same type twice must fail")
	}

	if _, ok := Get("Dup"); ok {
		t.Error("Get by an ambiguous name must report not found")
	}
	s1, ok1 := SchemaOf(first)
	s2, ok2 := SchemaOf(second)
	if !ok1 || !ok2 || s1.Collection != "dup_one" || s2.Collection != "dup_two" {
		t.Errorf("SchemaOf resolved wrong schemas: %v %v", s1, s2)
	}
	if s, err := getSchemaForModel([]*Dup{}); err != nil || s.Collection != "dup_one" {
		t.Errorf("slice resolution: %v %v", s, err)
	}

	all := GetAll()
	if len(all) < 2 {
		t.Errorf("GetAll should include both: %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Collection > all[i].Collection {
			t.Errorf("GetAll not sorted by collection: %s > %s", all[i-1].Collection, all[i].Collection)
		}
	}
}

// --- custom ID type ---

type testStringID struct {
	ID   string `bson:"_id"`
	Name string `bson:"name" goodm:"required"`
}

func TestCustomStringID_Lifecycle(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testStringID{}, "test_string_ids", "")

	if err := Create(ctx, &testStringID{Name: "no id"}); err == nil {
		t.Fatal("Create with a zero string _id must fail instead of inserting an empty key")
	}

	doc := &testStringID{ID: "alpha", Name: "Alpha"}
	if err := Create(ctx, doc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var got testStringID
	if err := FindByID(ctx, "alpha", &got); err != nil || got.Name != "Alpha" {
		t.Fatalf("FindByID: %+v %v", got, err)
	}
	doc.Name = "Beta"
	if err := Update(ctx, doc); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := FindByID(ctx, "alpha", &got); err != nil || got.Name != "Beta" {
		t.Fatalf("after Update: %+v %v", got, err)
	}
	if err := UpdateFields(ctx, doc, bson.M{"name": "Gamma"}); err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}
	if err := Delete(ctx, doc); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := FindByID(ctx, "alpha", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFindByID_AcceptsHexString(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	u := seedUsers(t, ctx, 1)[0]

	var got testUser
	if err := FindByID(ctx, u.ID.Hex(), &got); err != nil || got.ID != u.ID {
		t.Fatalf("hex lookup: %v", err)
	}
	if err := FindByID(ctx, "not-hex", &got); err == nil {
		t.Fatal("invalid hex must error")
	}
}

// --- query operations ---

func TestQueryOperations(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	users := seedUsers(t, ctx, 5)

	n, err := Count(ctx, bson.D{{Key: "role", Value: "admin"}}, &testUser{})
	if err != nil || n != 3 {
		t.Errorf("Count: %d %v", n, err)
	}
	ok, err := Exists(ctx, bson.D{{Key: "email", Value: "u1@test.com"}}, &testUser{})
	if err != nil || !ok {
		t.Errorf("Exists true: %v %v", ok, err)
	}
	ok, err = Exists(ctx, bson.D{{Key: "email", Value: "nobody"}}, &testUser{})
	if err != nil || ok {
		t.Errorf("Exists false: %v %v", ok, err)
	}
	var roles []string
	if err := Distinct(ctx, "role", bson.D{}, &testUser{}, &roles); err != nil || len(roles) != 2 {
		t.Errorf("Distinct: %v %v", roles, err)
	}

	var projected []testUser
	err = Find(ctx, bson.D{}, &projected, FindOptions{Projection: bson.D{{Key: "email", Value: 1}}, Limit: 2})
	if err != nil || len(projected) != 2 || projected[0].Name != "" || projected[0].Email == "" {
		t.Errorf("Projection: %+v %v", projected, err)
	}

	// FindOneAndUpdate returns the new document, manages updated_at and __v.
	var updated testUser
	err = FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: users[0].ID}},
		bson.M{"$set": bson.M{"age": 99}},
		&updated)
	if err != nil || updated.Age != 99 || updated.Version != 1 || !updated.UpdatedAt.After(users[0].UpdatedAt) {
		t.Errorf("FindOneAndUpdate: %+v %v", updated, err)
	}
	var original testUser
	err = FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: users[0].ID}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "age", Value: 1}}}},
		&original, FindOneAndUpdateOptions{ReturnOriginal: true})
	if err != nil || original.Age != 99 {
		t.Errorf("ReturnOriginal: %+v %v", original, err)
	}
	err = FindOneAndUpdate(ctx, bson.D{{Key: "email", Value: "missing"}}, bson.M{"$set": bson.M{"age": 1}}, &updated)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("FindOneAndUpdate miss: %v", err)
	}

	// Upsert inserts with created_at.
	var upserted testUser
	err = FindOneAndUpdate(ctx,
		bson.D{{Key: "email", Value: "new@test.com"}},
		bson.M{"$set": bson.M{"name": "New", "age": 30}},
		&upserted, FindOneAndUpdateOptions{Upsert: true})
	if err != nil || upserted.ID.IsZero() || upserted.CreatedAt.IsZero() || upserted.Version != 1 {
		t.Errorf("Upsert: %+v %v", upserted, err)
	}

	// UpdateOne with Upsert and UpdateMany results.
	if err := UpdateOne(ctx, bson.D{{Key: "email", Value: "up@test.com"}}, bson.M{"$set": bson.M{"name": "Up"}}, &testUser{}, Upsert()); err != nil {
		t.Errorf("UpdateOne upsert: %v", err)
	}
	if ok, _ := Exists(ctx, bson.D{{Key: "email", Value: "up@test.com"}}, &testUser{}); !ok {
		t.Error("UpdateOne upsert did not insert")
	}

	// FindOneAndReplace and FindOneAndDelete.
	replacement := &testUser{Email: "u1@test.com", Name: "Replaced", Age: 50}
	replacement.ID = users[1].ID
	var replaced testUser
	if err := FindOneAndReplace(ctx, bson.D{{Key: "_id", Value: users[1].ID}}, replacement, &replaced); err != nil || replaced.Name != "Replaced" {
		t.Errorf("FindOneAndReplace: %+v %v", replaced, err)
	}
	var deleted testUser
	if err := FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: users[2].ID}}, &deleted); err != nil || deleted.ID != users[2].ID {
		t.Errorf("FindOneAndDelete: %+v %v", deleted, err)
	}
	if ok, _ := Exists(ctx, bson.D{{Key: "_id", Value: users[2].ID}}, &testUser{}); ok {
		t.Error("FindOneAndDelete did not delete")
	}
	if err := FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: users[2].ID}}, &deleted); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindOneAndDelete miss: %v", err)
	}
}

func TestWithManagedUpdate(t *testing.T) {
	out := withManagedUpdate(bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 1}}}}, fixedTime, true).(bson.D)
	set, _ := operatorDoc(out, "$set")
	inc, _ := operatorDoc(out, "$inc")
	soi, _ := operatorDoc(out, "$setOnInsert")
	if len(set) != 2 || set[1].Key != "updated_at" {
		t.Errorf("$set: %v", set)
	}
	if len(inc) != 1 || inc[0].Key != "__v" {
		t.Errorf("$inc: %v", inc)
	}
	if len(soi) != 1 || soi[0].Key != "created_at" {
		t.Errorf("$setOnInsert: %v", soi)
	}

	// Caller-managed fields are respected.
	out = withManagedUpdate(bson.M{"$set": bson.M{"updated_at": fixedTime, "__v": 7, "created_at": fixedTime}}, fixedTime, true).(bson.D)
	if d, _ := operatorDoc(out, "$inc"); d != nil {
		t.Errorf("should not $inc __v when caller sets it: %v", d)
	}
	if d, _ := operatorDoc(out, "$setOnInsert"); d != nil {
		t.Errorf("should not add $setOnInsert when caller sets created_at: %v", d)
	}

	// Pipelines pass through untouched.
	pipe := []bson.D{{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}}}
	if _, same := withManagedUpdate(pipe, fixedTime, false).([]bson.D); !same {
		t.Error("pipeline update should pass through")
	}
}

// --- transactions ---

func TestWithTransaction_NestedJoinsOuter(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	outer := &testUser{Email: "outer@test.com", Name: "Outer"}
	inner := &testUser{Email: "inner@test.com", Name: "Inner"}
	err := WithTransaction(ctx, func(ctx context.Context) error {
		if err := Create(ctx, outer); err != nil {
			return err
		}
		if err := WithTransaction(ctx, func(ctx context.Context) error {
			return Create(ctx, inner)
		}); err != nil {
			return err
		}
		return errors.New("abort outer")
	})
	if err == nil {
		t.Fatal("expected the outer transaction to fail")
	}
	n, _ := Count(ctx, bson.D{}, &testUser{})
	if n != 0 {
		t.Fatalf("nested transaction committed independently: %d documents survived the outer abort", n)
	}
}
