package goodm

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dwoolworth/goodm/internal"
)

var (
	registryMu sync.RWMutex
	registry   = map[reflect.Type]*Schema{} // keyed by the model's struct type
	byName     = map[string][]*Schema{}     // ModelName -> schemas (may collide across packages)
	byColl     = map[string][]*Schema{}     // collection -> schemas
)

// Register parses a model struct and registers its schema.
// The model should be a pointer to a struct that embeds goodm.Model.
// The collection parameter is the MongoDB collection name.
//
// Schemas are keyed by struct type, so two packages may each register a type
// with the same name. Get looks schemas up by name and fails if that name is
// ambiguous; SchemaOf looks them up by type and never is.
func Register(model interface{}, collection string) error {
	t := modelType(model)
	if t == nil || t.Kind() != reflect.Struct {
		return fmt.Errorf("goodm: Register expects a struct, got %T", model)
	}

	schema := &Schema{
		ModelName:  t.Name(),
		PkgPath:    t.PkgPath(),
		Type:       t,
		Collection: collection,
	}

	// Parse struct fields (recursively handles subdocuments)
	if bad := invalidInlineField(t, nil); bad != "" {
		return fmt.Errorf("goodm: model %s: field %s is tagged bson:\",inline\" but inline fields must be a struct, a struct pointer, or a map", schema.QualifiedName(), bad)
	}
	schema.Fields = parseFields(t, nil)
	if dup := duplicateBSONName(schema.Fields); dup != "" {
		return fmt.Errorf("goodm: model %s has two fields stored as %q; the driver keeps only one", schema.QualifiedName(), dup)
	}
	schema.idField = findIDField(t)
	schema.createdAt, schema.updatedAt, schema.version = findManagedFields(schema.Fields)
	if schema.idField == nil {
		if embedsModelWithoutInline(t) {
			return fmt.Errorf("goodm: model %s embeds goodm.Model without the bson:\",inline\" tag, so its _id would be stored nested; embed it as goodm.Model with the tag bson:\",inline\"", schema.QualifiedName())
		}
		return fmt.Errorf("goodm: model %s has no field tagged bson:\"_id\" (embed goodm.Model with bson:\",inline\" or tag an ID field)", schema.QualifiedName())
	}

	// Check for Indexable interface (compound indexes)
	if indexable, ok := model.(Indexable); ok {
		schema.CompoundIndexes = indexable.Indexes()
	}

	// Check for Configurable interface (per-schema collection options)
	if configurable, ok := model.(Configurable); ok {
		schema.CollOptions = configurable.CollectionOptions()
	}

	// Detect hook implementations
	schema.Hooks = detectHooks(model)

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[t]; exists {
		return fmt.Errorf("goodm: model %q is already registered", schema.QualifiedName())
	}
	registry[t] = schema
	byName[schema.ModelName] = append(byName[schema.ModelName], schema)
	byColl[collection] = append(byColl[collection], schema)
	return nil
}

// Unregister removes a model's schema from the registry. It is a no-op for
// unregistered models. Useful in tests that register models per case.
func Unregister(model interface{}) {
	t := modelType(model)
	if t == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	schema, ok := registry[t]
	if !ok {
		return
	}
	delete(registry, t)
	byName[schema.ModelName] = removeSchema(byName[schema.ModelName], schema)
	if len(byName[schema.ModelName]) == 0 {
		delete(byName, schema.ModelName)
	}
	byColl[schema.Collection] = removeSchema(byColl[schema.Collection], schema)
	if len(byColl[schema.Collection]) == 0 {
		delete(byColl, schema.Collection)
	}
}

func removeSchema(list []*Schema, s *Schema) []*Schema {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// GetAll returns all registered schemas, ordered by collection name and then
// by qualified model name so output is stable.
func GetAll() []*Schema {
	registryMu.RLock()
	defer registryMu.RUnlock()

	result := make([]*Schema, 0, len(registry))
	for _, s := range registry {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Collection != result[j].Collection {
			return result[i].Collection < result[j].Collection
		}
		return result[i].QualifiedName() < result[j].QualifiedName()
	})
	return result
}

// Get returns the schema registered under the given model (struct) name.
// It returns false if no model has that name, or if more than one does; use
// SchemaOf to resolve a model by type in that case.
func Get(name string) (*Schema, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	list := byName[name]
	if len(list) != 1 {
		return nil, false
	}
	return list[0], true
}

// SchemaOf returns the schema for a model value, pointer, or slice of either
// (e.g. &User{}, User{}, []User, *[]*User), resolved by struct type.
func SchemaOf(model interface{}) (*Schema, bool) {
	t := modelType(model)
	if t == nil {
		return nil, false
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	s, ok := registry[t]
	return s, ok
}

// schemaForCollection returns the schema registered for a collection, if any.
// When several models share a collection the first registered wins.
func schemaForCollection(collection string) (*Schema, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	list := byColl[collection]
	if len(list) == 0 {
		return nil, false
	}
	return list[0], true
}

// modelType unwraps pointers and slices to reach the underlying struct type
// of a model value. Returns nil if the value is not struct-based.
func modelType(model interface{}) reflect.Type {
	if model == nil {
		return nil
	}
	t, ok := model.(reflect.Type)
	if !ok {
		t = reflect.TypeOf(model)
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

// parseFields recursively parses struct fields into FieldSchema slices.
// The seen map tracks types being parsed to prevent infinite recursion on circular references.
func parseFields(t reflect.Type, seen map[reflect.Type]bool) []FieldSchema {
	if seen == nil {
		seen = make(map[reflect.Type]bool)
	}

	fields := internal.StructFields(t)
	var result []FieldSchema

	for _, f := range fields {
		bsonTag := f.Tag.Get("bson")
		bsonName, _ := ParseBSONTag(bsonTag)
		if bsonName == "" {
			bsonName = strings.ToLower(f.Name)
		}
		if bsonName == "-" {
			continue
		}

		goodmTag := f.Tag.Get("goodm")
		fs := ParseGoodmTag(goodmTag)
		fs.Name = f.Name
		fs.BSONName = bsonName
		fs.Type = internal.TypeName(f.Type)
		fs.goType = f.Type
		fs.index = f.Index

		// Determine underlying type (deref pointers, unwrap slices)
		fieldType := f.Type
		isSlice := false
		if fieldType.Kind() == reflect.Slice {
			isSlice = true
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}

		// Recurse into struct types that aren't leaf types
		if fieldType.Kind() == reflect.Struct && !isLeafType(fieldType) {
			fs.IsSlice = isSlice
			if !seen[fieldType] {
				seen[fieldType] = true
				fs.SubFields = parseFields(fieldType, seen)
				delete(seen, fieldType)
			}
		}

		result = append(result, fs)
	}

	return result
}

// findIDField locates the field tagged bson:"_id" among the fields the driver
// stores at the top level (see internal.StructFields) and returns its index
// path. Returns nil if there is none.
func findIDField(t reflect.Type) []int {
	for _, f := range internal.StructFields(t) {
		if name, _ := ParseBSONTag(f.Tag.Get("bson")); name == "_id" {
			return f.Index
		}
	}
	return nil
}

// findManagedFields locates the fields goodm maintains by the bson names the
// documents use, so a promoted Go field of the same name elsewhere in the
// struct cannot be mistaken for them.
func findManagedFields(fields []FieldSchema) (createdAt, updatedAt, version []int) {
	for _, f := range fields {
		switch {
		case f.BSONName == "created_at" && f.goType == reflect.TypeOf(time.Time{}):
			createdAt = f.index
		case f.BSONName == "updated_at" && f.goType == reflect.TypeOf(time.Time{}):
			updatedAt = f.index
		case f.BSONName == "__v" && f.goType != nil && isIntKind(f.goType.Kind()):
			version = f.index
		}
	}
	return
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// invalidInlineField returns the Go name of the first exported field, at any
// depth, tagged bson:",inline" whose type the driver cannot inline (anything
// but a struct, a struct pointer, or a map), or "". The driver would reject
// the model on its first write; reporting it at Register is kinder.
func invalidInlineField(t reflect.Type, seen map[reflect.Type]bool) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || isLeafType(t) || seen[t] {
		return ""
	}
	if seen == nil {
		seen = map[reflect.Type]bool{}
	}
	seen[t] = true
	defer delete(seen, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if bsonTagInline(f.Tag.Get("bson")) {
			// The driver inlines a struct, a pointer to a struct, or a map
			// (but not a pointer to a map).
			inlineOK := ft.Kind() == reflect.Struct || (f.Type.Kind() == reflect.Map)
			if !inlineOK {
				return f.Name
			}
		}
		if ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
			ft = ft.Elem()
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
		}
		if ft.Kind() == reflect.Struct {
			if bad := invalidInlineField(ft, seen); bad != "" {
				return f.Name + "." + bad
			}
		}
	}
	return ""
}

// duplicateBSONName returns a bson path two visible fields share, at any
// depth, or "". The driver stores only one of them, so such a model is
// ambiguous.
func duplicateBSONName(fields []FieldSchema) string {
	return duplicateBSONNameAt(fields, "")
}

func duplicateBSONNameAt(fields []FieldSchema, prefix string) string {
	seen := map[string]bool{}
	for _, f := range fields {
		if seen[f.BSONName] {
			return prefix + f.BSONName
		}
		seen[f.BSONName] = true
		if len(f.SubFields) > 0 {
			if dup := duplicateBSONNameAt(f.SubFields, prefix+f.BSONName+"."); dup != "" {
				return dup
			}
		}
	}
	return ""
}

// embedsModelWithoutInline reports whether t embeds goodm.Model without the
// bson:",inline" tag, the most likely cause of a missing _id.
func embedsModelWithoutInline(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == reflect.TypeOf(Model{}) && !bsonTagInline(f.Tag.Get("bson")) {
			return true
		}
	}
	return false
}

// detectHooks checks which hook interfaces a model implements.
func detectHooks(model interface{}) []string {
	var hooks []string
	if _, ok := model.(BeforeCreate); ok {
		hooks = append(hooks, "BeforeCreate")
	}
	if _, ok := model.(AfterCreate); ok {
		hooks = append(hooks, "AfterCreate")
	}
	if _, ok := model.(BeforeSave); ok {
		hooks = append(hooks, "BeforeSave")
	}
	if _, ok := model.(AfterSave); ok {
		hooks = append(hooks, "AfterSave")
	}
	if _, ok := model.(BeforeDelete); ok {
		hooks = append(hooks, "BeforeDelete")
	}
	if _, ok := model.(AfterDelete); ok {
		hooks = append(hooks, "AfterDelete")
	}
	return hooks
}
