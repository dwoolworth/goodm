package goodm

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// CompoundIndex declares an index on a MongoDB collection. Despite the name it
// may cover a single field; use it whenever a field tag cannot express the
// index you need (descending keys, partial filters, collation, custom names).
//
// Each entry in Fields is a bson field path with an optional direction or
// type marker:
//
//	"email"           ascending
//	"-created_at"     descending
//	"title:text"      text index
//	"tenant:hashed"   hashed index
//	"loc:2dsphere"    geospatial index
//
// Return a slice of these from a model's Indexes() method (the Indexable
// interface) and Enforce or Migrate will create them.
type CompoundIndex struct {
	Fields []string
	Unique bool
	Sparse bool

	// Name overrides MongoDB's default index name (e.g. "email_1_role_-1").
	Name string

	// ExpireAfterSeconds makes this a TTL index. Use TTL() to build the pointer
	// from a time.Duration. A value of 0 expires documents at the indexed date.
	ExpireAfterSeconds *int32

	// PartialFilter restricts the index to documents matching this filter
	// (a bson.D or bson.M). Unique partial indexes enforce uniqueness only
	// among matching documents.
	PartialFilter interface{}

	// Collation sets the collation for string comparison in the index.
	Collation *options.Collation
}

// NewCompoundIndex creates a non-unique index on the given fields.
func NewCompoundIndex(fields ...string) CompoundIndex {
	return CompoundIndex{Fields: fields, Unique: false}
}

// NewUniqueCompoundIndex creates a unique index on the given fields.
func NewUniqueCompoundIndex(fields ...string) CompoundIndex {
	return CompoundIndex{Fields: fields, Unique: true}
}

// TTL converts a duration into the *int32 seconds value expected by
// CompoundIndex.ExpireAfterSeconds. Sub-second durations round down to 0.
func TTL(d time.Duration) *int32 {
	s := int32(d / time.Second)
	return &s
}

// IndexDefinition is the fully resolved description of an index a schema
// expects to exist. Enforce, Migrate, and the CLI all derive their work from
// this single type, so a field tag and an Indexes() entry that describe the
// same index are treated identically.
type IndexDefinition struct {
	Name               string
	Keys               bson.D
	Unique             bool
	Sparse             bool
	ExpireAfterSeconds *int32
	PartialFilter      interface{}
	Collation          *options.Collation
}

// Model converts the definition into a driver IndexModel.
func (d IndexDefinition) Model() mongo.IndexModel {
	opts := options.Index().SetName(d.Name)
	if d.Unique {
		opts.SetUnique(true)
	}
	if d.Sparse {
		opts.SetSparse(true)
	}
	if d.ExpireAfterSeconds != nil {
		opts.SetExpireAfterSeconds(*d.ExpireAfterSeconds)
	}
	if d.PartialFilter != nil {
		opts.SetPartialFilterExpression(d.PartialFilter)
	}
	if d.Collation != nil {
		opts.SetCollation(d.Collation)
	}
	return mongo.IndexModel{Keys: d.Keys, Options: opts}
}

// IndexDefinitions returns every index the schema expects, derived from field
// tags (including dotted paths into subdocuments) and the model's Indexes()
// method. Definitions appear in declaration order: field indexes first, then
// compound indexes.
func (s *Schema) IndexDefinitions() []IndexDefinition {
	var defs []IndexDefinition
	defs = appendFieldIndexes(defs, s.Fields, "")
	for _, ci := range s.CompoundIndexes {
		defs = append(defs, ci.definition())
	}
	return defs
}

// appendFieldIndexes walks fields recursively, emitting a definition for each
// field tagged index, unique, or ttl. prefix is the dotted path to the field.
func appendFieldIndexes(defs []IndexDefinition, fields []FieldSchema, prefix string) []IndexDefinition {
	for _, f := range fields {
		path := f.BSONName
		if prefix != "" {
			path = prefix + "." + f.BSONName
		}
		if f.Unique || f.Index || f.TTL != nil {
			kind := f.IndexKind
			if kind == "" {
				kind = "1"
			}
			def := IndexDefinition{
				Keys:               bson.D{{Key: path, Value: indexKeyValue(kind)}},
				Unique:             f.Unique,
				Sparse:             f.Sparse,
				ExpireAfterSeconds: f.TTL,
			}
			def.Name = DefaultIndexName(def.Keys)
			defs = append(defs, def)
		}
		if len(f.SubFields) > 0 {
			defs = appendFieldIndexes(defs, f.SubFields, path)
		}
	}
	return defs
}

// definition resolves a CompoundIndex into an IndexDefinition.
func (ci CompoundIndex) definition() IndexDefinition {
	keys := ci.Keys()
	name := ci.Name
	if name == "" {
		name = DefaultIndexName(keys)
	}
	return IndexDefinition{
		Name:               name,
		Keys:               keys,
		Unique:             ci.Unique,
		Sparse:             ci.Sparse,
		ExpireAfterSeconds: ci.ExpireAfterSeconds,
		PartialFilter:      normalizePartialFilter(ci.PartialFilter),
		Collation:          ci.Collation,
	}
}

// normalizePartialFilter converts a partial filter into its canonical ordered
// form before it is created or compared, so a filter written with unordered
// maps is stored by the server in a deterministic order and later matches.
func normalizePartialFilter(f interface{}) interface{} {
	if f == nil {
		return nil
	}
	c, err := canonicalBSON(f)
	if err != nil {
		return f
	}
	return c
}

// Keys returns the index key document described by Fields, resolving the
// direction and type markers.
func (ci CompoundIndex) Keys() bson.D {
	keys := make(bson.D, 0, len(ci.Fields))
	for _, f := range ci.Fields {
		field, kind := parseIndexField(f)
		keys = append(keys, bson.E{Key: field, Value: indexKeyValue(kind)})
	}
	return keys
}

// parseIndexField splits an Indexes() field entry into its path and kind
// ("1", "-1", "text", "hashed", "2dsphere", "2d").
func parseIndexField(f string) (field, kind string) {
	if path, k, ok := strings.Cut(f, ":"); ok {
		return path, k
	}
	if strings.HasPrefix(f, "-") {
		return f[1:], "-1"
	}
	return f, "1"
}

// indexKeyValue converts an index kind to the value MongoDB expects in the
// key document: int32 for directions, string for special index types.
func indexKeyValue(kind string) interface{} {
	switch kind {
	case "", "1", "asc":
		return int32(1)
	case "-1", "desc":
		return int32(-1)
	default:
		return kind
	}
}

// DefaultIndexName returns the name MongoDB assigns to an index with these
// keys when none is given: key names and values joined by underscores, e.g.
// "email_1", "tenant_1_created_at_-1", "title_text".
func DefaultIndexName(keys bson.D) string {
	parts := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		parts = append(parts, k.Key, fmt.Sprint(k.Value))
	}
	return strings.Join(parts, "_")
}

// compoundIndexName returns the resolved name of a CompoundIndex.
func compoundIndexName(ci CompoundIndex) string {
	return ci.definition().Name
}

// parseTTL parses a ttl= tag value: either a Go duration ("24h", "30m") or a
// plain number of seconds.
func parseTTL(v string) (*int32, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return TTL(d), nil
	}
	var secs int32
	if _, err := fmt.Sscanf(v, "%d", &secs); err != nil {
		return nil, fmt.Errorf("goodm: invalid ttl %q (use seconds or a duration like 24h)", v)
	}
	return &secs, nil
}

// indexKeysEqual compares two index key documents by field, direction, and
// type. The text fields of a compound text index are compared as a set,
// because the server reports them in alphabetical order regardless of how
// the index was declared.
func indexKeysEqual(a, b bson.D) bool {
	a, b = canonicalTextKeys(a), canonicalTextKeys(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || indexKeyString(a[i].Value) != indexKeyString(b[i].Value) {
			return false
		}
	}
	return true
}

// canonicalTextKeys gathers every text key into one alphabetically sorted run
// placed where the first text key appeared. Non-text keys keep their order.
func canonicalTextKeys(keys bson.D) bson.D {
	var text []string
	for _, k := range keys {
		if indexKeyString(k.Value) == "text" {
			text = append(text, k.Key)
		}
	}
	if len(text) < 2 {
		return keys
	}
	sort.Strings(text)
	out := make(bson.D, 0, len(keys))
	placed := false
	for _, k := range keys {
		if indexKeyString(k.Value) != "text" {
			out = append(out, k)
			continue
		}
		if !placed {
			for _, f := range text {
				out = append(out, bson.E{Key: f, Value: "text"})
			}
			placed = true
		}
	}
	return out
}

// indexKeyString normalizes an index key value (direction int or type string)
// so int, int32, int64, float64 and string forms compare equal.
func indexKeyString(v interface{}) string {
	switch n := v.(type) {
	case int:
		return fmt.Sprint(n)
	case int32:
		return fmt.Sprint(n)
	case int64:
		return fmt.Sprint(n)
	case float64:
		return fmt.Sprint(int64(n))
	case string:
		return n
	}
	return fmt.Sprint(v)
}

// partialFiltersEqual compares a desired partial filter against the raw
// expression reported by the server, after canonicalizing both.
func partialFiltersEqual(desired interface{}, existing bson.Raw) bool {
	if desired == nil && len(existing) == 0 {
		return true
	}
	if desired == nil || len(existing) == 0 {
		return false
	}
	want, err := canonicalBSON(desired)
	if err != nil {
		return false
	}
	have, err := canonicalBSON(existing)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(want, have)
}

// filtersEquivalent compares two filter documents from Go code canonically.
func filtersEquivalent(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ca, errA := canonicalBSON(a)
	cb, errB := canonicalBSON(b)
	return errA == nil && errB == nil && reflect.DeepEqual(ca, cb)
}

// canonicalBSON round-trips a document through BSON and returns a form in
// which documents have sorted keys and all numbers are float64, so two ways
// of writing the same filter (bson.M vs bson.D, int vs int64) compare equal.
func canonicalBSON(v interface{}) (interface{}, error) {
	// Sort unordered maps before marshaling: once a bson.M has been encoded
	// its random order is indistinguishable from a deliberate bson.D order.
	raw, err := bson.Marshal(bson.D{{Key: "v", Value: canonicalValueAt(v, true)}})
	if err != nil {
		return nil, err
	}
	var holder struct {
		V interface{} `bson:"v"`
	}
	if err := bson.Unmarshal(raw, &holder); err != nil {
		return nil, err
	}
	return canonicalValue(holder.V), nil
}

func canonicalValue(v interface{}) interface{} {
	return canonicalValueAt(v, true)
}

// canonicalValueAt canonicalizes v. Keys are sorted only where MongoDB treats
// order as irrelevant: the top-level filter and documents made of operators
// ($gt, $in, ...). A subdocument under a field name is an exact-match value,
// where order is significant, so it keeps its order. Integers stay exact;
// a float that is an integer within 2^53 becomes an int64 so 5 and 5.0
// compare equal, while larger values keep their type.
func canonicalValueAt(v interface{}, top bool) interface{} {
	switch x := v.(type) {
	case bson.D:
		out := make(bson.D, len(x))
		allOps := len(x) > 0
		for i, e := range x {
			out[i] = bson.E{Key: e.Key, Value: canonicalValueAt(e.Value, isFilterListOperator(e.Key))}
			if !strings.HasPrefix(e.Key, "$") {
				allOps = false
			}
		}
		if top || allOps {
			sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		}
		return out
	case bson.M:
		return canonicalMap(x)
	case map[string]interface{}:
		return canonicalMap(x)
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = canonicalValueAt(e, top)
		}
		return out
	case []interface{}:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = canonicalValueAt(e, top)
		}
		return out
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float32:
		return canonicalFloat(float64(x))
	case float64:
		return canonicalFloat(x)
	}
	return v
}

// canonicalMap converts an unordered map into a key-sorted bson.D.
func canonicalMap(m map[string]interface{}) bson.D {
	out := make(bson.D, 0, len(m))
	for k, val := range m {
		out = append(out, bson.E{Key: k, Value: canonicalValueAt(val, isFilterListOperator(k))})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// isFilterListOperator reports whether an operator's array holds filter
// documents (whose key order is irrelevant) rather than values.
func isFilterListOperator(key string) bool {
	return key == "$and" || key == "$or" || key == "$nor"
}

func canonicalFloat(f float64) interface{} {
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return int64(f)
	}
	return f
}

// collationsEquivalent compares two collations from Go code with the server's
// defaults filled in, so {Locale: "en"} equals {Locale: "en", Strength: 3}.
func collationsEquivalent(a, b *options.Collation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return normalizeCollation(*a) == normalizeCollation(*b)
}

func normalizeCollation(c options.Collation) options.Collation {
	if c.Strength == 0 {
		c.Strength = 3
	}
	if c.CaseFirst == "" {
		c.CaseFirst = "off"
	}
	if c.Alternate == "" {
		c.Alternate = "non-ignorable"
	}
	if c.MaxVariable == "" {
		c.MaxVariable = "punct"
	}
	return c
}

// collationsEqual reports whether the collation the server stores satisfies
// the desired one. The server fills in defaults for every unset option, so
// only the options the schema sets explicitly are compared.
func collationsEqual(desired *options.Collation, existing bson.Raw) bool {
	if desired == nil && len(existing) == 0 {
		return true
	}
	if desired == nil || len(existing) == 0 {
		return false
	}
	var have struct {
		Locale          string `bson:"locale"`
		CaseLevel       bool   `bson:"caseLevel"`
		CaseFirst       string `bson:"caseFirst"`
		Strength        int    `bson:"strength"`
		NumericOrdering bool   `bson:"numericOrdering"`
		Alternate       string `bson:"alternate"`
		MaxVariable     string `bson:"maxVariable"`
		Normalization   bool   `bson:"normalization"`
		Backwards       bool   `bson:"backwards"`
	}
	if err := bson.Unmarshal(existing, &have); err != nil {
		return false
	}
	if desired.Locale != have.Locale {
		return false
	}
	if desired.Strength != 0 && desired.Strength != have.Strength {
		return false
	}
	if desired.CaseLevel && !have.CaseLevel {
		return false
	}
	if desired.CaseFirst != "" && desired.CaseFirst != have.CaseFirst {
		return false
	}
	if desired.NumericOrdering && !have.NumericOrdering {
		return false
	}
	if desired.Alternate != "" && desired.Alternate != have.Alternate {
		return false
	}
	if desired.MaxVariable != "" && desired.MaxVariable != have.MaxVariable {
		return false
	}
	if desired.Normalization && !have.Normalization {
		return false
	}
	if desired.Backwards && !have.Backwards {
		return false
	}
	return true
}
