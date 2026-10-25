package goodm

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Refs maps ref field paths to destination pointers for population.
// Keys are bson names of fields tagged goodm:"ref=collection", or dotted
// paths to such fields inside subdocuments ("address.country",
// "items.product"); a numeric segment selects one element of a slice
// ("items.0.product", "tags.1").
type Refs map[string]interface{}

// PopulateOptions configures the Populate operation.
type PopulateOptions struct {
	DB *mongo.Database
}

// Populate resolves ref fields on a loaded model by fetching referenced documents
// from their respective collections. Each key in refs is the path of a field
// tagged goodm:"ref=collection", and the corresponding value is a pointer to a
// struct or slice where the referenced documents will be decoded.
//
// For single refs (bson.ObjectID), the target should be a pointer to a struct:
//
//	profile := &Profile{}
//	err := goodm.Populate(ctx, user, goodm.Refs{"profile": profile})
//
// For array refs ([]bson.ObjectID), the target should be a pointer to a slice:
//
//	var tags []Tag
//	err := goodm.Populate(ctx, post, goodm.Refs{"tags": &tags})
//
// A ref inside a subdocument is named by its dotted path. When the path
// crosses a slice of subdocuments, the ids of every element are fetched
// together, so the target must be a pointer to a slice:
//
//	var products []Product
//	err := goodm.Populate(ctx, order, goodm.Refs{"items.product": &products})
//
// A numeric segment picks one element instead: "items.0.product" names a
// single ref and takes a pointer to a struct.
func Populate(ctx context.Context, model interface{}, refs Refs, opts ...PopulateOptions) error {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return err
	}

	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return err
	}

	v := reflect.ValueOf(model)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}

	for path, target := range refs {
		steps, err := resolveRefPath(schema, path)
		if err != nil {
			return err
		}
		leaf := steps[len(steps)-1].field
		coll := collectionByName(db, leaf.Ref)

		c := idCollector{}
		if err := c.collect(v, steps); err != nil {
			return fmt.Errorf("goodm: ref %q: %w", path, err)
		}

		tv := reflect.ValueOf(target)
		if tv.Kind() != reflect.Pointer || tv.IsNil() {
			return fmt.Errorf("goodm: populate target for %q must be a non-nil pointer, got %T", path, target)
		}
		if tv.Elem().Kind() == reflect.Slice {
			if err := populateArrayRef(ctx, coll, c.ids, path, target); err != nil {
				return err
			}
			continue
		}
		if c.many {
			return fmt.Errorf("goodm: ref %q yields several ids; the target must be a pointer to a slice, got %T", path, target)
		}
		if len(c.ids) == 0 {
			continue // unset ref: leave the target untouched
		}
		if err := populateSingleRef(ctx, coll, c.ids[0], path, target); err != nil {
			return err
		}
	}

	return nil
}

// refStep is one field along a ref path. index selects one element when the
// field is a slice, or is -1 for every element.
type refStep struct {
	field *FieldSchema
	index int
}

// resolveRefPath resolves a dotted path of bson names (with optional numeric
// element segments) to the ref field it ends at.
func resolveRefPath(schema *Schema, path string) ([]refStep, error) {
	segs := strings.Split(path, ".")
	fields := schema.Fields
	var steps []refStep
	for i, seg := range segs {
		if n, err := strconv.Atoi(seg); err == nil && len(steps) > 0 && isSliceType(steps[len(steps)-1].field.goType) && steps[len(steps)-1].index < 0 {
			if n < 0 {
				return nil, fmt.Errorf("goodm: ref path %q has a negative index", path)
			}
			steps[len(steps)-1].index = n
			continue
		}
		if fields == nil {
			return nil, fmt.Errorf("goodm: field %q in ref path %q is not a subdocument", segs[i-1], path)
		}
		field := fieldIn(fields, seg)
		if field == nil {
			if len(segs) == 1 {
				return nil, fmt.Errorf("goodm: field %q not found in schema for %s", path, schema.ModelName)
			}
			return nil, fmt.Errorf("goodm: field %q in ref path %q not found in schema for %s", seg, path, schema.ModelName)
		}
		steps = append(steps, refStep{field: field, index: -1})
		fields = field.SubFields
	}
	leaf := steps[len(steps)-1].field
	if leaf.Ref == "" {
		return nil, fmt.Errorf("goodm: field %q has no ref tag", path)
	}
	return steps, nil
}

func fieldIn(fields []FieldSchema, bsonName string) *FieldSchema {
	for i := range fields {
		if fields[i].BSONName == bsonName {
			return &fields[i]
		}
	}
	return nil
}

func isSliceType(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Slice || t.Kind() == reflect.Array
}

// idCollector gathers the distinct non-zero ObjectIDs found at a ref path.
// many reports that the path can hold more than one id: it crosses a slice
// of subdocuments without an index, or ends at an array ref.
type idCollector struct {
	ids  []bson.ObjectID
	seen map[bson.ObjectID]bool
	many bool
}

func (c *idCollector) add(id bson.ObjectID) {
	if id.IsZero() {
		return
	}
	if c.seen == nil {
		c.seen = map[bson.ObjectID]bool{}
	}
	if c.seen[id] {
		return
	}
	c.seen[id] = true
	c.ids = append(c.ids, id)
}

// collect walks one model value along the steps.
func (c *idCollector) collect(v reflect.Value, steps []refStep) error {
	step := steps[0]
	fv := schemaField(v, step.field)
	if !fv.IsValid() {
		return nil // behind a nil inline embed: absent
	}
	if len(steps) == 1 {
		return c.leaf(fv, step)
	}

	rest := steps[1:]
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil
		}
		fv = fv.Elem()
	}
	switch fv.Kind() {
	case reflect.Slice, reflect.Array:
		if step.index >= 0 {
			if step.index >= fv.Len() {
				return nil
			}
			return c.collectElem(fv.Index(step.index), rest)
		}
		c.many = true
		for i := 0; i < fv.Len(); i++ {
			if err := c.collectElem(fv.Index(i), rest); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		return c.collect(fv, rest)
	}
	return fmt.Errorf("field %q is not a subdocument", step.field.BSONName)
}

func (c *idCollector) collectElem(ev reflect.Value, rest []refStep) error {
	if ev.Kind() == reflect.Pointer {
		if ev.IsNil() {
			return nil
		}
		ev = ev.Elem()
	}
	if ev.Kind() != reflect.Struct {
		return fmt.Errorf("elements of %q are not subdocuments", rest[0].field.BSONName)
	}
	return c.collect(ev, rest)
}

// leaf reads the ids from the ref field itself.
func (c *idCollector) leaf(fv reflect.Value, step refStep) error {
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil
		}
		fv = fv.Elem()
	}
	switch x := fv.Interface().(type) {
	case bson.ObjectID:
		c.add(x)
		return nil
	case []bson.ObjectID:
		if step.index >= 0 {
			if step.index < len(x) {
				c.add(x[step.index])
			}
			return nil
		}
		c.many = true
		for _, id := range x {
			c.add(id)
		}
		return nil
	}
	return fmt.Errorf("field %q is not bson.ObjectID or []bson.ObjectID", step.field.BSONName)
}

// collectionByName opens a collection by name, applying the collection options
// of the model registered for it (if any) so populated reads honor the
// referenced model's read preference and concern.
func collectionByName(db *mongo.Database, name string) *mongo.Collection {
	if schema, ok := schemaForCollection(name); ok {
		return getCollection(db, schema)
	}
	return db.Collection(name)
}

// populateArrayRef fetches all documents whose IDs are in ids using a single
// $in query. With no ids the target slice is left as it is.
func populateArrayRef(ctx context.Context, coll *mongo.Collection, ids []bson.ObjectID, path string, target interface{}) error {
	if len(ids) == 0 {
		return nil
	}
	cursor, err := coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return fmt.Errorf("goodm: populate %q failed: %w", path, err)
	}
	if err := cursor.All(ctx, target); err != nil {
		_ = cursor.Close(ctx)
		return fmt.Errorf("goodm: populate %q decode failed: %w", path, err)
	}
	_ = cursor.Close(ctx)
	return nil
}

// populateSingleRef fetches a single document by its ObjectID.
func populateSingleRef(ctx context.Context, coll *mongo.Collection, refID bson.ObjectID, path string, target interface{}) error {
	if refID.IsZero() {
		return nil // skip unset refs
	}
	if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: refID}}).Decode(target); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil // referenced document not found, leave target as zero
		}
		return fmt.Errorf("goodm: populate %q failed: %w", path, err)
	}
	return nil
}

// BatchPopulate resolves a single ref field across a slice of models in one query.
// It collects unique IDs from the ref field and fetches all referenced documents
// using a single $in query, avoiding N+1 overhead.
//
// models must be a slice or pointer to a slice (e.g. []Post or *[]Post).
// field is the path of the ref field (e.g. "author", or "items.product" for
// a ref inside a subdocument; see Refs).
// results must be a pointer to a slice of the referenced type (e.g. *[]User).
//
// Example:
//
//	var posts []Post
//	goodm.Find(ctx, bson.D{}, &posts)
//
//	var authors []User
//	err := goodm.BatchPopulate(ctx, posts, "author", &authors)
func BatchPopulate(ctx context.Context, models interface{}, field string, results interface{}, opts ...PopulateOptions) error {
	// Validate results is *[]T
	rv := reflect.ValueOf(results)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("goodm: results must be a pointer to a slice, got %T", results)
	}

	// Normalize models to a reflect.Value of a slice
	mv := reflect.ValueOf(models)
	if mv.Kind() == reflect.Pointer {
		mv = mv.Elem()
	}
	if mv.Kind() != reflect.Slice {
		return fmt.Errorf("goodm: models must be a slice, got %T", models)
	}
	if mv.Len() == 0 {
		return nil
	}

	schema, err := getSchemaForModel(models)
	if err != nil {
		return err
	}
	steps, err := resolveRefPath(schema, field)
	if err != nil {
		return err
	}

	c := idCollector{}
	for i := 0; i < mv.Len(); i++ {
		el := mv.Index(i)
		if el.Kind() == reflect.Pointer {
			if el.IsNil() {
				continue
			}
			el = el.Elem()
		}
		if err := c.collect(el, steps); err != nil {
			return fmt.Errorf("goodm: ref %q: %w", field, err)
		}
	}
	if len(c.ids) == 0 {
		return nil
	}

	// Fetch all referenced documents in one query
	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return err
	}

	coll := collectionByName(db, steps[len(steps)-1].field.Ref)
	cursor, err := coll.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: c.ids}}}})
	if err != nil {
		return fmt.Errorf("goodm: batch populate %q failed: %w", field, err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	if err := cursor.All(ctx, results); err != nil {
		return fmt.Errorf("goodm: batch populate decode failed: %w", err)
	}

	return nil
}
