package goodm

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// cents is stored as a string by a codec registered on the client.
type cents struct{ N int }

type codecModel struct {
	Model `bson:",inline"`
	Price cents `bson:"price"`
}

func (m *codecModel) CollectionOptions() CollectionOptions {
	return CollectionOptions{SkipValidator: true}
}

func centsRegistry() *bson.Registry {
	reg := bson.NewRegistry()
	reg.RegisterTypeEncoder(reflect.TypeOf(cents{}), bson.ValueEncoderFunc(func(_ bson.EncodeContext, vw bson.ValueWriter, v reflect.Value) error {
		return vw.WriteString(strconv.Itoa(int(v.Field(0).Int())))
	}))
	reg.RegisterTypeDecoder(reflect.TypeOf(cents{}), bson.ValueDecoderFunc(func(_ bson.DecodeContext, vr bson.ValueReader, v reflect.Value) error {
		s, err := vr.ReadString()
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		v.Field(0).SetInt(int64(n))
		return nil
	}))
	return reg
}

func TestWatchDecodeUsesClientCodecs(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	client, err := mongo.Connect(options.Client().ApplyURI(mongoURIForTests()).SetRegistry(centsRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(ctx) }()
	cdb := client.Database(db.Name())
	if err := Register(&codecModel{}, "codec_models"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&codecModel{})

	s := openStream(t, ctx, &codecModel{}, WatchOptions{DB: cdb})
	m := &codecModel{Price: cents{N: 125}}
	if err := Create(ctx, m, CreateOptions{DB: cdb}); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, s)
	if v, _ := ev.FullDocument.LookupErr("price"); v.Type != bson.TypeString {
		t.Fatalf("stored price is %v, want a string", v)
	}
	var got codecModel
	if err := s.Decode(&got); err != nil {
		t.Fatalf("stream Decode: %v", err)
	}
	if got.Price.N != 125 || got.ID != m.ID {
		t.Errorf("decoded %+v", got)
	}
	// The detached event cannot know the codec.
	if err := ev.Decode(&codecModel{}); err == nil {
		t.Error("ChangeEvent.Decode with a client codec should fail")
	}
	// The merge base was recorded, as after FindOne. (WithRetry itself
	// cannot merge a model stored through client codecs; that is a
	// documented limitation of the merge, not of the stream.)
	if modelBase(mustSchema(t, &got), &got) == nil {
		t.Error("stream Decode recorded no merge base")
	}
	got.Price.N = 130
	if err := Update(ctx, &got, UpdateOptions{DB: cdb}); err != nil {
		t.Errorf("plain update after stream decode: %v", err)
	}

	// SkipValidator keeps Enforce off this collection.
	if err := Enforce(ctx, cdb, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if cv, _ := readCollectionValidation(ctx, cdb, "codec_models"); len(cv.validator) != 0 {
		t.Error("validator applied despite SkipValidator")
	}
	if err := Enforce(ctx, cdb, EnforceOptions{Validator: ValidatorVerify}); err != nil {
		t.Errorf("Verify with SkipValidator: %v", err)
	}
	// Pre-image decode through the stream.
	if err := s.DecodeBefore(&got); !errors.Is(err, ErrNoFullDocument) {
		t.Errorf("DecodeBefore without pre-image = %v", err)
	}
}

func mongoURIForTests() string {
	if uri := os.Getenv("MONGODB_URI"); uri != "" {
		return uri
	}
	return "mongodb://localhost:27017"
}

type namedByte uint8

type byteModel struct {
	Model `bson:",inline"`
	Raw   []byte       `bson:"raw"`
	Named []namedByte  `bson:"named"`
	Fixed [3]namedByte `bson:"fixed"`
}

func TestJSONSchemaNamedByteTypes(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	if err := Register(&byteModel{}, "byte_models"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&byteModel{})
	s, _ := SchemaOf(&byteModel{})
	props := lookup(t, s.JSONSchema(), "properties").(bson.D)
	if got := lookup(t, props, "raw", "bsonType"); !bsonValuesEqual(got, bson.A{"binData", "null"}) {
		t.Errorf("[]byte = %v", got)
	}
	if got := lookup(t, props, "named", "bsonType"); !bsonValuesEqual(got, bson.A{"array", "null"}) {
		t.Errorf("[]namedByte = %v", got)
	}
	if got := lookup(t, props, "fixed", "bsonType"); got != "array" {
		t.Errorf("[3]namedByte = %v", got)
	}
	if err := Enforce(ctx, db, EnforceOptions{Validator: ValidatorApply}); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, &byteModel{Raw: []byte{1}, Named: []namedByte{2}, Fixed: [3]namedByte{3, 4, 5}}); err != nil {
		t.Errorf("named byte types rejected: %v", err)
	}
}

func TestUpdateFieldsEnumElementZero(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	type enumTags struct {
		Model `bson:",inline"`
		Tags  []string `bson:"tags" goodm:"enum=a|b"`
	}
	if err := Register(&enumTags{}, "enum_tags"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&enumTags{})
	m := &enumTags{Tags: []string{"a", "b"}}
	if err := Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	var ve ValidationErrors
	if err := UpdateFields(ctx, m, bson.M{"tags.1": nil}); !errors.As(err, &ve) {
		t.Errorf("nil element of an enum slice = %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"tags.1": ""}); !errors.As(err, &ve) {
		t.Errorf("empty element of an enum slice = %v", err)
	}
	if err := UpdateFields(ctx, m, bson.M{"tags.1": "a"}); err != nil {
		t.Errorf("valid element = %v", err)
	}
	if errs := Validate(m, mustSchema(t, m)); len(errs) != 0 {
		t.Errorf("model invalid after UpdateFields: %v", errs)
	}
}

func mustSchema(t *testing.T, m interface{}) *Schema {
	t.Helper()
	s, ok := SchemaOf(m)
	if !ok {
		t.Fatalf("%T not registered", m)
	}
	return s
}

func TestPopulateChecksAllRefsFirst(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	author := &testUser{Email: "a@x.com", Name: "A"}
	if err := Create(ctx, author); err != nil {
		t.Fatal(err)
	}
	post := &testPost{Title: "p", AuthorID: author.ID}
	if err := Create(ctx, post); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		got := &testUser{}
		err := Populate(ctx, post, Refs{"author": got, "tags": &testTag{}})
		if err == nil {
			t.Fatal("bad target accepted")
		}
		if got.Name != "" {
			t.Fatalf("author populated despite a bad entry in the same call")
		}
	}
}
