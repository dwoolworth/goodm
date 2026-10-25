package goodm

import (
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// CollectionOptions configures per-schema MongoDB collection behavior.
// Implement the Configurable interface on your model to set these.
type CollectionOptions struct {
	ReadPreference *readpref.ReadPref
	ReadConcern    *readconcern.ReadConcern
	WriteConcern   *writeconcern.WriteConcern

	// SkipValidator makes Enforce leave this collection's validator alone
	// under every ValidatorPolicy, for a model whose stored form the schema
	// cannot describe (for example one using codecs registered on the
	// client).
	SkipValidator bool
}

// FieldSchema describes a single field parsed from struct tags.
type FieldSchema struct {
	Name      string        // Go field name
	BSONName  string        // bson tag name
	Type      string        // Go type as string
	Required  bool          // field must be non-zero
	Unique    bool          // unique index on this field
	Index     bool          // single-field index
	IndexKind string        // index key kind: "" or "1" (ascending), "-1", "hashed", "text", "2dsphere"
	Sparse    bool          // index skips documents missing the field
	TTL       *int32        // expireAfterSeconds for a TTL index on this field
	Default   string        // raw default value
	Enum      []string      // allowed values
	Min       *float64      // minimum value (numbers) or length (strings)
	Max       *float64      // maximum value (numbers) or length (strings)
	Ref       string        // referenced collection
	Immutable bool          // cannot be changed after creation
	SubFields []FieldSchema // inner fields for struct/[]struct subdocuments
	IsSlice   bool          // true if field is []struct or []*struct

	goType reflect.Type // the Go type of the field, used to convert values set via UpdateFields
	index  []int        // reflect index path from the enclosing struct, through inline embeds
}

// GoType returns the Go type of the field.
func (f *FieldSchema) GoType() reflect.Type { return f.goType }

// isLeafType returns true for struct types that serialize as atomic BSON values
// and should NOT be recursed into for subdocument parsing.
func isLeafType(t reflect.Type) bool {
	return t == reflect.TypeOf(time.Time{}) ||
		t == reflect.TypeOf(bson.ObjectID{}) ||
		t == reflect.TypeOf(bson.Decimal128{})
}

// Schema is the parsed representation of a model struct.
type Schema struct {
	ModelName       string            // Go struct name
	PkgPath         string            // import path of the package defining the struct
	Type            reflect.Type      // the model's struct type
	Collection      string            // MongoDB collection name
	Fields          []FieldSchema     // parsed fields
	CompoundIndexes []CompoundIndex   // compound indexes from Indexes() method
	Hooks           []string          // hook interface names the model implements
	CollOptions     CollectionOptions // per-schema read/write concern and read preference

	idField   []int // reflect field index path of the _id field, nil if none
	createdAt []int // index path of the time.Time field stored as created_at, if any
	updatedAt []int // index path of the time.Time field stored as updated_at, if any
	version   []int // index path of the integer field stored as __v, if any
	modelPath []int // index path of the embedded goodm.Model, nil if the struct has none
}

// QualifiedName returns the package path and struct name, e.g.
// "github.com/acme/app/models.User". It is unique across the registry.
func (s *Schema) QualifiedName() string {
	if s.PkgPath == "" {
		return s.ModelName
	}
	return s.PkgPath + "." + s.ModelName
}

// HasField returns true if the schema contains a field with the given BSON name.
func (s *Schema) HasField(bsonName string) bool {
	for _, f := range s.Fields {
		if f.BSONName == bsonName {
			return true
		}
	}
	return false
}

// GetField returns the FieldSchema for a given BSON name, or nil if not found.
func (s *Schema) GetField(bsonName string) *FieldSchema {
	for i := range s.Fields {
		if s.Fields[i].BSONName == bsonName {
			return &s.Fields[i]
		}
	}
	return nil
}

// Indexable is implemented by models that define compound indexes.
type Indexable interface {
	Indexes() []CompoundIndex
}

// Configurable is implemented by models that define per-schema collection options
// such as read preference, read concern, and write concern.
//
// Example:
//
//	func (u *User) CollectionOptions() goodm.CollectionOptions {
//	    return goodm.CollectionOptions{
//	        ReadPreference: readpref.SecondaryPreferred(),
//	        WriteConcern:   writeconcern.Majority(),
//	    }
//	}
type Configurable interface {
	CollectionOptions() CollectionOptions
}
