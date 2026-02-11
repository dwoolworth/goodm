package goodm

// FieldSchema describes a single field parsed from struct tags.
type FieldSchema struct {
	Name      string   // Go field name
	BSONName  string   // bson tag name
	Type      string   // Go type as string
	Required  bool     // field must be non-zero
	Unique    bool     // unique index on this field
	Index     bool     // single-field index
	Default   string   // raw default value
	Enum      []string // allowed values
	Min       *int     // minimum value/length
	Max       *int     // maximum value/length
	Ref       string   // referenced collection
	Immutable bool     // cannot be changed after creation
}

// Schema is the parsed representation of a model struct.
type Schema struct {
	ModelName       string          // Go struct name
	Collection      string          // MongoDB collection name
	Fields          []FieldSchema   // parsed fields
	CompoundIndexes []CompoundIndex // compound indexes from Indexes() method
	Hooks           []string        // hook interface names the model implements
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
