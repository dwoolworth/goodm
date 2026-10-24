package goodm

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

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
	schema.Fields = parseFields(t, nil)
	schema.idField = findIDField(t)

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

// findIDField locates the struct field that maps to "_id", searching through
// embedded structs. It prefers an explicit bson:"_id" tag and falls back to a
// field named ID. Returns nil if the struct has no identifiable ID field.
func findIDField(t reflect.Type) []int {
	var fallback []int
	var walk func(t reflect.Type, prefix []int) []int
	walk = func(t reflect.Type, prefix []int) []int {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			idx := append(append([]int{}, prefix...), i)
			name, _ := ParseBSONTag(f.Tag.Get("bson"))
			if name == "_id" {
				return idx
			}
			if f.Anonymous {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					if found := walk(ft, idx); found != nil {
						return found
					}
				}
				continue
			}
			if f.Name == "ID" && fallback == nil {
				fallback = idx
			}
		}
		return nil
	}
	if found := walk(t, nil); found != nil {
		return found
	}
	return fallback
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
