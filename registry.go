package goodm

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/dwoolworth/goodm/internal"
)

var (
	registryMu sync.RWMutex
	registry   = map[string]*Schema{}
)

// Register parses a model struct and registers its schema.
// The model should be a pointer to a struct that embeds goodm.Model.
// The collection parameter is the MongoDB collection name.
func Register(model interface{}, collection string) error {
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("goodm: Register expects a struct, got %s", t.Kind())
	}

	schema := &Schema{
		ModelName:  t.Name(),
		Collection: collection,
	}

	// Parse struct fields
	fields := internal.StructFields(t)
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

		schema.Fields = append(schema.Fields, fs)
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
	if _, exists := registry[schema.ModelName]; exists {
		registryMu.Unlock()
		return fmt.Errorf("goodm: model %q is already registered", schema.ModelName)
	}
	registry[schema.ModelName] = schema
	registryMu.Unlock()

	return nil
}

// GetAll returns all registered schemas.
func GetAll() map[string]*Schema {
	registryMu.RLock()
	defer registryMu.RUnlock()

	result := make(map[string]*Schema, len(registry))
	for k, v := range registry {
		result[k] = v
	}
	return result
}

// Get returns the schema for a given model name, or false if not found.
func Get(name string) (*Schema, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	s, ok := registry[name]
	return s, ok
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
