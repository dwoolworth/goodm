package goodm

// CompoundIndex represents a multi-field index on a MongoDB collection.
type CompoundIndex struct {
	Fields []string
	Unique bool
}

// NewCompoundIndex creates a non-unique compound index on the given fields.
func NewCompoundIndex(fields ...string) CompoundIndex {
	return CompoundIndex{Fields: fields, Unique: false}
}

// NewUniqueCompoundIndex creates a unique compound index on the given fields.
func NewUniqueCompoundIndex(fields ...string) CompoundIndex {
	return CompoundIndex{Fields: fields, Unique: true}
}
