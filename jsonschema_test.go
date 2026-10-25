package goodm

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type jsLine struct {
	SKU   string  `bson:"sku"   goodm:"required"`
	Qty   int32   `bson:"qty"   goodm:"min=1,max=100"`
	Price float64 `bson:"price" goodm:"min=0"`
}

type jsAddress struct {
	Street string `bson:"street" goodm:"required"`
	Zip    string `bson:"zip"    goodm:"min=5,max=5"`
}

type jsModel struct {
	Model    `bson:",inline"`
	Name     string            `bson:"name"     goodm:"required,min=1,max=64"`
	Role     string            `bson:"role"     goodm:"enum=admin|user"`
	Level    int               `bson:"level"    goodm:"enum=1|2|3,min=1,max=3"`
	Ratio    float32           `bson:"ratio"    goodm:"min=0,max=1"`
	Count    uint16            `bson:"count"`
	Big      uint64            `bson:"big"      goodm:"min=10"`
	Flag     bool              `bson:"flag"`
	When     time.Time         `bson:"when"`
	Ref      bson.ObjectID     `bson:"ref"      goodm:"ref=others"`
	Refs     []bson.ObjectID   `bson:"refs"     goodm:"max=3"`
	Tags     []string          `bson:"tags"     goodm:"enum=a|b|c,min=1"`
	Blob     []byte            `bson:"blob"`
	Nick     *string           `bson:"nick,omitempty" goodm:"max=8"`
	Addr     jsAddress         `bson:"addr"     goodm:"required"`
	AddrPtr  *jsAddress        `bson:"addr_ptr"`
	Lines    []jsLine          `bson:"lines"    goodm:"min=1"`
	Meta     map[string]int    `bson:"meta"     goodm:"max=4"`
	Any      interface{}       `bson:"any"`
	Doc      bson.D            `bson:"doc"`
	M        bson.M            `bson:"m"`
	Dec      bson.Decimal128   `bson:"dec"`
	Fixed    [2]int64          `bson:"fixed"`
	BadEnum  int               `bson:"bad_enum" goodm:"enum=x|y"`
	Skip     string            `bson:"-"`
	PtrLines []*jsLine         `bson:"ptr_lines"`
	Nested   map[string]jsLine `bson:"nested"`
}

// lookup follows a dotted path through nested bson.D/bson.A values.
func lookup(t *testing.T, v interface{}, path ...string) interface{} {
	t.Helper()
	for _, p := range path {
		d, ok := v.(bson.D)
		if !ok {
			t.Fatalf("path %v: %T is not a document", path, v)
		}
		found := false
		for _, e := range d {
			if e.Key == p {
				v, found = e.Value, true
				break
			}
		}
		if !found {
			t.Fatalf("path %v: key %q missing in %v", path, p, d)
		}
	}
	return v
}

func has(d bson.D, key string) bool {
	for _, e := range d {
		if e.Key == key {
			return true
		}
	}
	return false
}

func TestJSONSchemaShape(t *testing.T) {
	if err := Register(&jsModel{}, "js_models"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&jsModel{})
	s, _ := SchemaOf(&jsModel{})
	js := s.JSONSchema()

	if lookup(t, js, "title") != "jsModel" || lookup(t, js, "bsonType") != "object" {
		t.Errorf("header: %v", js)
	}
	req, _ := lookup(t, js, "required").([]string)
	if len(req) != 2 || req[0] != "name" || req[1] != "addr" {
		t.Errorf("required = %v", req)
	}
	props := lookup(t, js, "properties").(bson.D)
	if has(props, "Skip") || has(props, "-") {
		t.Error("bson:\"-\" field emitted")
	}

	type want struct {
		path []string
		val  interface{}
	}
	checks := []want{
		{[]string{"_id", "bsonType"}, "objectId"},
		{[]string{"created_at", "bsonType"}, "date"},
		{[]string{"__v", "bsonType"}, bson.A{"int", "long"}},
		{[]string{"name", "bsonType"}, "string"},
		{[]string{"name", "minLength"}, int64(1)},
		{[]string{"name", "maxLength"}, int64(64)},
		{[]string{"role", "enum"}, bson.A{"admin", "user", ""}},
		{[]string{"level", "bsonType"}, bson.A{"int", "long"}},
		{[]string{"level", "enum"}, bson.A{int64(1), int64(2), int64(3), int64(0)}},
		{[]string{"level", "minimum"}, 1.0},
		{[]string{"level", "maximum"}, 3.0},
		{[]string{"ratio", "bsonType"}, "double"},
		{[]string{"ratio", "maximum"}, 1.0},
		{[]string{"count", "bsonType"}, "int"},
		{[]string{"count", "minimum"}, 0.0},
		{[]string{"big", "bsonType"}, bson.A{"int", "long"}},
		{[]string{"big", "minimum"}, 10.0},
		{[]string{"flag", "bsonType"}, "bool"},
		{[]string{"when", "bsonType"}, "date"},
		{[]string{"ref", "bsonType"}, "objectId"},
		{[]string{"refs", "bsonType"}, bson.A{"array", "null"}},
		{[]string{"refs", "items", "bsonType"}, "objectId"},
		{[]string{"refs", "maxItems"}, int64(3)},
		{[]string{"tags", "items", "bsonType"}, "string"},
		{[]string{"tags", "items", "enum"}, bson.A{"a", "b", "c"}},
		{[]string{"tags", "minItems"}, int64(1)},
		{[]string{"blob", "bsonType"}, bson.A{"binData", "null"}},
		{[]string{"nick", "bsonType"}, bson.A{"string", "null"}},
		{[]string{"nick", "maxLength"}, int64(8)},
		{[]string{"addr", "bsonType"}, "object"},
		{[]string{"addr", "properties", "street", "bsonType"}, "string"},
		{[]string{"addr", "properties", "zip", "minLength"}, int64(5)},
		{[]string{"addr", "required"}, []string{"street"}},
		{[]string{"addr_ptr", "bsonType"}, bson.A{"object", "null"}},
		{[]string{"addr_ptr", "required"}, []string{"street"}},
		{[]string{"lines", "bsonType"}, bson.A{"array", "null"}},
		{[]string{"lines", "minItems"}, int64(1)},
		{[]string{"lines", "items", "bsonType"}, "object"},
		{[]string{"lines", "items", "required"}, []string{"sku"}},
		{[]string{"lines", "items", "properties", "qty", "bsonType"}, "int"},
		{[]string{"lines", "items", "properties", "qty", "minimum"}, 1.0},
		{[]string{"lines", "items", "properties", "price", "minimum"}, 0.0},
		{[]string{"meta", "bsonType"}, bson.A{"object", "null"}},
		{[]string{"meta", "additionalProperties", "bsonType"}, bson.A{"int", "long"}},
		{[]string{"meta", "maxProperties"}, int64(4)},
		{[]string{"doc", "bsonType"}, bson.A{"object", "null"}},
		{[]string{"m", "bsonType"}, bson.A{"object", "null"}},
		{[]string{"dec", "bsonType"}, "decimal"},
		{[]string{"fixed", "bsonType"}, "array"},
		{[]string{"fixed", "items", "bsonType"}, bson.A{"int", "long"}},
		{[]string{"ptr_lines", "items", "bsonType"}, bson.A{"object", "null"}},
		{[]string{"nested", "additionalProperties", "bsonType"}, "object"},
	}
	for _, c := range checks {
		got := lookup(t, props, c.path...)
		if !bsonValuesEqual(got, c.val) {
			t.Errorf("%v = %#v, want %#v", c.path, got, c.val)
		}
	}
	if any := lookup(t, props, "any").(bson.D); len(any) != 0 {
		t.Errorf("interface{} field constrained: %v", any)
	}
	if bad := lookup(t, props, "bad_enum").(bson.D); has(bad, "enum") {
		t.Errorf("unparseable enum emitted: %v", bad)
	}
	if nested := lookup(t, props, "nested", "additionalProperties").(bson.D); has(nested, "properties") {
		t.Errorf("map element struct should not carry properties: %v", nested)
	}

	// The document round-trips through the server as a validator, and the
	// canonical comparison sees it as unchanged.
	raw, err := bson.Marshal(bson.D{{Key: "$jsonSchema", Value: js}})
	if err != nil {
		t.Fatal(err)
	}
	eq, err := documentsEquivalent(bson.Raw(raw), bson.D{{Key: "$jsonSchema", Value: js}})
	if err != nil || !eq {
		t.Errorf("self-equivalence: %v %v", eq, err)
	}
}

func TestDocumentsEquivalent(t *testing.T) {
	a := bson.D{{Key: "x", Value: bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: bson.A{1.0, "s"}}}}, {Key: "y", Value: "z"}}
	b := bson.M{"y": "z", "x": bson.M{"a": bson.A{int64(1), "s"}, "b": 1.0}}
	if eq, err := documentsEquivalent(a, b); err != nil || !eq {
		t.Errorf("reordered/widened docs not equivalent: %v %v", eq, err)
	}
	c := bson.D{{Key: "x", Value: bson.D{{Key: "b", Value: int32(1)}, {Key: "a", Value: bson.A{"s", 1.0}}}}, {Key: "y", Value: "z"}}
	if eq, _ := documentsEquivalent(a, c); eq {
		t.Error("array order must matter")
	}
}

func TestEnforceValidatorApplyAndVerify(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Register(&jsModel{}, "js_models"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&jsModel{})

	// Verify before anything exists: an error naming the missing collection.
	err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorVerify})
	var ee *EnforcementError
	if !errors.As(err, &ee) {
		t.Fatalf("verify on empty db = %v", err)
	}

	// Apply creates the collections that do not exist with the validator...
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cv, err := readCollectionValidation(ctx, db, "js_models")
	if err != nil || !cv.exists || len(cv.validator) == 0 {
		t.Fatalf("after apply: %+v %v", cv, err)
	}
	// ...and verify now passes, with default level and action.
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorVerify}); err != nil {
		t.Fatalf("verify after apply: %v", err)
	}
	// A different level is a mismatch for verify and is applied by apply.
	err = Enforce(ctx, db, EnforceOptions{Validator: ValidatorVerify, ValidationLevel: ValidationLevelModerate})
	if err == nil {
		t.Fatal("verify should see the level change")
	}
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply, ValidationLevel: ValidationLevelModerate, ValidationAction: ValidationActionWarn}); err != nil {
		t.Fatal(err)
	}
	cv, _ = readCollectionValidation(ctx, db, "js_models")
	if cv.level != "moderate" || cv.action != "warn" {
		t.Errorf("level/action = %q/%q", cv.level, cv.action)
	}
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorVerify, ValidationLevel: ValidationLevelModerate, ValidationAction: ValidationActionWarn}); err != nil {
		t.Errorf("verify moderate/warn: %v", err)
	}

	// A hand-edited validator is reported by verify and replaced by apply.
	cmd := bson.D{{Key: "collMod", Value: "js_models"}, {Key: "validator", Value: bson.D{{Key: "$jsonSchema", Value: bson.D{{Key: "bsonType", Value: "object"}}}}}}
	if err := db.RunCommand(ctx, cmd).Err(); err != nil {
		t.Fatal(err)
	}
	err = Enforce(ctx, db, EnforceOptions{Validator: ValidatorVerify, ValidationLevel: ValidationLevelModerate, ValidationAction: ValidationActionWarn})
	if !errors.As(err, &ee) || ee.Collection != "js_models" {
		t.Fatalf("verify after edit = %v", err)
	}
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatal(err)
	}
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorVerify}); err != nil {
		t.Errorf("verify after re-apply: %v", err)
	}

	// Apply on an existing collection with data: the validator is set via
	// collMod, and the default policy leaves it alone.
	if err := Enforce(ctx, db); err != nil {
		t.Fatal(err)
	}
	cv, _ = readCollectionValidation(ctx, db, "js_models")
	if len(cv.validator) == 0 {
		t.Error("ValidatorIgnore removed the validator")
	}
}

func TestEnforceValidatorSharedCollection(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	type otherUser struct {
		Model `bson:",inline"`
		Email string `bson:"email"`
	}
	if err := Register(&otherUser{}, "test_users"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&otherUser{})
	err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply})
	var ee *EnforcementError
	if !errors.As(err, &ee) || ee.Collection != "test_users" {
		t.Fatalf("shared collection = %v", err)
	}
	if err := Enforce(ctx, db); err != nil {
		t.Errorf("ValidatorIgnore must not care about sharing: %v", err)
	}
}

func TestDocumentValidationErrors(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatal(err)
	}

	// goodm's own validation agrees with the validator, so make the
	// validator stricter than the struct (age at least 5): writes goodm
	// accepts then fail at the server.
	u := &testUser{Email: "v@x.com", Name: "V", Age: 1}
	if err := Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	many := []*testUser{{Email: "m1@x.com", Name: "M", Age: 1}, {Email: "m2@x.com", Name: "M", Age: 1}}
	for _, m := range many {
		if err := Create(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	strict := bson.D{{Key: "$jsonSchema", Value: bson.D{{Key: "properties", Value: bson.D{{Key: "age", Value: bson.D{{Key: "minimum", Value: 5}}}}}}}}
	cmd := bson.D{{Key: "collMod", Value: "test_users"}, {Key: "validator", Value: strict}}
	if err := db.RunCommand(ctx, cmd).Err(); err != nil {
		t.Fatal(err)
	}
	err := UpdateFields(ctx, u, bson.M{"age": 2})
	var dve *DocumentValidationError
	if !errors.Is(err, ErrDocumentValidation) || !errors.As(err, &dve) {
		t.Fatalf("UpdateFields = %v", err)
	}
	if dve.Collection != "test_users" {
		t.Errorf("collection = %q", dve.Collection)
	}
	if _, lookupErr := dve.Details.LookupErr("details"); lookupErr != nil {
		t.Errorf("details missing errInfo: %s", dve.Details)
	}
	if errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrNotFound) {
		t.Errorf("misclassified: %v", err)
	}
	if u.Age != 1 || u.Version != 0 {
		t.Errorf("failed UpdateFields changed the model: %+v", u)
	}

	u.Age = 2
	err = Update(ctx, u)
	if !errors.Is(err, ErrDocumentValidation) {
		t.Errorf("Update = %v", err)
	}
	var stored testUser
	if err := FindByID(ctx, u.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Version != u.Version || stored.Age != 1 {
		t.Errorf("stored %+v, model version %d", stored, u.Version)
	}
	err = SaveMany(ctx, many)
	var be *BulkError
	if !errors.As(err, &be) || len(be.Errors) != 2 || !errors.Is(be.Errors[0].Err, ErrDocumentValidation) {
		t.Errorf("SaveMany = %v", err)
	}
	if many[0].Version != 0 {
		t.Errorf("failed SaveMany left version %d", many[0].Version)
	}

	// Raw operations and the FindOneAnd* family (a command error).
	err = UpdateOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, bson.M{"$set": bson.M{"age": 0}}, &testUser{})
	if !errors.Is(err, ErrDocumentValidation) {
		t.Errorf("UpdateOne = %v", err)
	}
	var out testUser
	err = FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: u.ID}}, bson.M{"$set": bson.M{"age": 2}}, &out)
	if !errors.Is(err, ErrDocumentValidation) || !errors.As(err, &dve) {
		t.Errorf("FindOneAndUpdate = %v", err)
	}

	// Bulk paths report it per item.
	bad := &testUser{Email: "b@x.com", Name: "B", Age: 9}
	_, err = NewBulk(&testUser{}).
		InsertOne(bad).
		UpdateOne(bson.D{{Key: "_id", Value: u.ID}}, bson.M{"$set": bson.M{"age": 3}}).
		Execute(ctx)
	if !errors.As(err, &be) || len(be.Errors) != 1 || be.Errors[0].Index != 1 || !errors.Is(be.Errors[0].Err, ErrDocumentValidation) {
		t.Errorf("bulk = %v", err)
	}
	if _, err := UpdateMany(ctx, bson.D{}, bson.M{"$set": bson.M{"age": 4}}, &testUser{}); !errors.Is(err, ErrDocumentValidation) {
		t.Errorf("UpdateMany = %v", err)
	}
	// A document goodm writes normally passes the validator it generated,
	// including zero values of enum fields that have no default.
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatal(err)
	}
	ok := &testUser{Email: "ok@x.com", Name: "OK", Age: 30, Role: "admin"}
	if err := Create(ctx, ok); err != nil {
		t.Errorf("valid create rejected: %v", err)
	}
	ok.Age = 31
	if err := Update(ctx, ok); err != nil {
		t.Errorf("valid update rejected: %v", err)
	}
	order := &testOrder{Name: "o", Address: testAddress{Street: "s", City: "c"}, Items: []testOrderItem{{Name: "i", Quantity: 1}}}
	if err := Create(ctx, order); err != nil {
		t.Errorf("valid nested create rejected: %v", err)
	}
	if err := Register(&jsModel{}, "js_models"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&jsModel{})
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatal(err)
	}
	// Role is "" (enum without default, not required) and every nilable
	// field is nil: goodm accepts this document, so the validator must too.
	zero := &jsModel{Name: "z", Addr: jsAddress{Street: "s", Zip: "12345"}, Level: 1, Big: 10, Tags: []string{"a"}, Lines: []jsLine{{SKU: "a", Qty: 1}}}
	if err := Create(ctx, zero); err != nil {
		t.Errorf("zero-valued model rejected by its own validator: %v", err)
	}
	zero.Nick = new(string)
	zero.AddrPtr = &jsAddress{Street: "s", Zip: "54321"}
	zero.PtrLines = []*jsLine{nil, {SKU: "b", Qty: 2}}
	zero.Meta = map[string]int{"k": 1}
	if err := Update(ctx, zero); err != nil {
		t.Errorf("populated model rejected: %v", err)
	}
	if err := UpdateFields(ctx, zero, bson.M{"meta": nil, "nick": nil, "ptr_lines": nil, "addr_ptr": nil}); err != nil {
		t.Errorf("nulls for nilable fields rejected: %v", err)
	}
	if err := UpdateFields(ctx, zero, bson.M{"role": "root"}); !errors.Is(err, ErrDocumentValidation) && err == nil {
		t.Errorf("enum violation accepted: %v", err)
	}
}

// jsStatus encodes itself as a string, so the schema cannot type it.
type jsStatus int

func (s jsStatus) MarshalBSONValue() (byte, []byte, error) {
	t, data, err := bson.MarshalValue(map[jsStatus]string{0: "off", 1: "on"}[s])
	return byte(t), data, err
}

func (s *jsStatus) UnmarshalBSONValue(t byte, data []byte) error {
	var str string
	if err := bson.UnmarshalValue(bson.Type(t), data, &str); err != nil {
		return err
	}
	if str == "on" {
		*s = 1
	} else {
		*s = 0
	}
	return nil
}

// jsEdge gathers every shape the first review found a goodm-accepted write
// could fail its own validator with.
type jsEdge struct {
	Model   `bson:",inline"`
	Nick    *string         `bson:"nick"            goodm:"enum=a|b"`
	Tags    []*string       `bson:"tags"            goodm:"enum=a|b"`
	Fixed   [2]string       `bson:"fixed"           goodm:"enum=a|b"`
	Status  jsStatus        `bson:"status"`
	Site    url.URL         `bson:"site"`
	Code    bson.JavaScript `bson:"code"`
	Small   int64           `bson:"small,minsize"`
	USmall  uint32          `bson:"usmall,minsize"`
	Name    string          `bson:"name"            goodm:"min=3"`
	Ratio   float32         `bson:"ratio"           goodm:"enum=0.1|0.2"`
	Huge    string          `bson:"huge"            goodm:"max=Inf"`
	Plain   string          `bson:"plain"`
	When    *time.Time      `bson:"when,omitempty"  goodm:"required"`
	Version int64           `bson:"ver"`
	Flag    bool            `bson:"flag"  goodm:"enum=true|false"`
	Level   int             `bson:"level" goodm:"enum=0|1|1"`
	Mode    string          `bson:"mode"  goodm:"enum=a|"`
	StEnum  jsStatus        `bson:"st"    goodm:"enum=0|1"`
	Num     json.Number     `bson:"num"   goodm:"enum=1|2,min=1"`
	Any     interface{}     `bson:"any"   goodm:"required"`
}

type jsVersion64 struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	CreatedAt time.Time     `bson:"created_at"`
	UpdatedAt time.Time     `bson:"updated_at"`
	Version   int64         `bson:"__v"`
	Name      string        `bson:"name"`
}

func TestValidatorAcceptsEveryGoodmWrite(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Register(&jsEdge{}, "js_edges"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&jsEdge{})
	if err := Register(&jsVersion64{}, "js_v64"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&jsVersion64{})

	s, _ := SchemaOf(&jsEdge{})
	props := lookup(t, s.JSONSchema(), "properties").(bson.D)
	if status := lookup(t, props, "status").(bson.D); len(status) != 0 {
		t.Errorf("self-marshaling type constrained: %v", status)
	}
	if got := lookup(t, props, "site", "bsonType"); got != "string" {
		t.Errorf("url.URL = %v", got)
	}
	if got := lookup(t, props, "code", "bsonType"); got != "javascript" {
		t.Errorf("JavaScript = %v", got)
	}
	if got := lookup(t, props, "small", "bsonType"); !bsonValuesEqual(got, bson.A{"int", "long"}) {
		t.Errorf("int64 = %v", got)
	}
	if got := lookup(t, props, "nick", "enum"); !bsonValuesEqual(got, bson.A{"a", "b", "", nil}) {
		t.Errorf("pointer enum = %v", got)
	}
	if got := lookup(t, props, "tags", "items", "enum"); !bsonValuesEqual(got, bson.A{"a", "b", nil}) {
		t.Errorf("pointer element enum = %v", got)
	}
	if got := lookup(t, props, "fixed", "items", "enum"); !bsonValuesEqual(got, bson.A{"a", "b", ""}) {
		t.Errorf("array element enum = %v", got)
	}
	if got := lookup(t, props, "ratio", "enum"); !bsonValuesEqual(got, bson.A{float64(float32(0.1)), float64(float32(0.2)), 0.0}) {
		t.Errorf("float32 enum = %v", got)
	}
	if huge := lookup(t, props, "huge").(bson.D); has(huge, "maxLength") {
		t.Errorf("infinite bound emitted: %v", huge)
	}
	if got := lookup(t, props, "flag", "enum"); !bsonValuesEqual(got, bson.A{true, false}) {
		t.Errorf("bool enum = %v", got)
	}
	if got := lookup(t, props, "level", "enum"); !bsonValuesEqual(got, bson.A{int64(0), int64(1)}) {
		t.Errorf("repeated enum = %v", got)
	}
	if got := lookup(t, props, "mode", "enum"); !bsonValuesEqual(got, bson.A{"a", ""}) {
		t.Errorf("enum listing the empty string = %v", got)
	}
	if st := lookup(t, props, "st").(bson.D); len(st) != 0 {
		t.Errorf("enum on a self-marshaling type: %v", st)
	}
	if num := lookup(t, props, "num").(bson.D); has(num, "enum") || has(num, "minimum") {
		t.Errorf("constraints on json.Number: %v", num)
	}
	if p := Validate(&jsEdge{Any: (*time.Time)(nil)}, s); len(p) != 1 || p[0].Field != "name" && p[0].Field != "when" {
		// Any holds a typed nil: not zero to the driver, so only the other
		// required/min fields complain. Mainly: no panic.
		t.Logf("typed nil interface: %v", p)
	}

	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	a := "a"
	m := &jsEdge{
		Any:    (*time.Time)(nil),
		StEnum: 1,
		Num:    "1",
		Tags:   []*string{&a, nil},
		Status: 1,
		Site:   url.URL{Scheme: "https", Host: "x"},
		Code:   "function(){}",
		Small:  7,
		USmall: 7,
		Name:   "ééé", // 3 characters, 6 bytes
		Ratio:  0.1,
		When:   &now,
	}
	if err := Create(ctx, m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"plain": nil, "nick": nil, "name": "ééé"}); err != nil {
		t.Fatalf("UpdateFields nil: %v", err)
	}
	var stored bson.M
	if err := db.Collection("js_edges").FindOne(ctx, bson.D{{Key: "_id", Value: m.ID}}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored["plain"] != "" || stored["nick"] != nil {
		t.Errorf("nil stored as %#v / %#v", stored["plain"], stored["nick"])
	}
	m.Small = 1 << 40
	if err := Update(ctx, m); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// goodm's own checks moved to match the server.
	m.Name = "éé"
	if err := Create(ctx, &jsEdge{Name: "éé", When: &now, Any: 1}); err == nil {
		t.Error("two characters accepted for min=3")
	}
	zero := time.Time{}.In(time.FixedZone("X", 3600))
	if err := Create(ctx, &jsEdge{Name: "abc", When: &zero, Any: 1}); err == nil {
		t.Error("required *time.Time at the zero instant accepted")
	}

	// A managed __v of type int64 survives an upsert through FindOneAndUpdate.
	var v64 jsVersion64
	err := FindOneAndUpdate(ctx, bson.D{{Key: "name", Value: "up"}}, bson.M{"$set": bson.M{"name": "up"}}, &v64, FindOneAndUpdateOptions{Upsert: true})
	if err != nil {
		t.Fatalf("upsert with int64 __v: %v", err)
	}
}
