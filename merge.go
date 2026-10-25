package goodm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/dwoolworth/goodm/internal"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// saveWithRetry attempts a versioned save, optionally retrying with a 3-way
// field-level merge when a version conflict occurs. On conflict the model
// keeps its stale version, so a further save conflicts until it is reloaded.
func saveWithRetry(ctx context.Context, coll *mongo.Collection, schema *Schema, model interface{}, opt UpdateOptions, id interface{}, scope interface{}) error {
	// The merge base is the document as last loaded or saved through goodm,
	// not the model as it is now: the model already carries the caller's
	// changes, and diffing it against itself would find none.
	var base bson.M
	if opt.MaxRetries > 0 {
		if raw := modelBase(schema, model); raw != nil {
			var err error
			base, err = normalizeBase(schema, raw)
			if err != nil {
				return fmt.Errorf("goodm: failed to decode merge base: %w", err)
			}
			// The base must describe this very document at the version the
			// model is about to save from; otherwise the model was refreshed
			// by other means (a cursor decode, a raw unmarshal) and the base
			// is stale. Merging against a stale base reverts changes.
			if ver, _ := getModelVersion(model); versionIn(base) != int64(ver) || idKey(base["_id"]) != idKey(id) {
				base = nil
				setModelBase(schema, model, nil)
			}
		}
	}

	for attempt := 0; ; attempt++ {
		err := attemptSave(ctx, coll, schema, model, opt.Unset, id, scope)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrVersionConflict) {
			return err
		}

		// Version conflict — can we retry with merge? Without a base, or out
		// of retries, the model keeps its stale version so that a further
		// save conflicts again until the caller reloads or merges; refreshing
		// the version here would let the next plain Update overwrite the
		// other writer's changes.
		if base == nil || attempt >= opt.MaxRetries {
			return ErrVersionConflict
		}

		// 3-way merge: re-read DB state, detect conflicts, apply disjoint
		// changes. The re-read state becomes the base for the next round, so
		// the other writer's changes are not mistaken for ours on a second
		// conflict.
		theirs, err := mergeFromDB(ctx, coll, schema, model, base, id, scope, opt.Unset)
		if err != nil {
			return err
		}
		base = theirs
	}
}

// normalizeBase decodes a stored document into the model type and back into
// the same map form diffFields compares, so differences between the stored
// document and the struct (missing or extra fields, integer widths, key
// order) are not mistaken for changes.
func normalizeBase(schema *Schema, raw bson.Raw) (bson.M, error) {
	doc := reflect.New(schema.Type).Interface()
	if err := bson.Unmarshal(raw, doc); err != nil {
		return nil, err
	}
	return toBsonMap(doc)
}

// attemptSave performs a single versioned replace. Returns ErrVersionConflict
// if the version filter did not match, or ErrNotFound if the document is gone.
func attemptSave(ctx context.Context, coll *mongo.Collection, schema *Schema, model interface{}, unsetFields []string, id interface{}, scope interface{}) error {
	oldVersion, _ := getModelVersion(model)
	oldUpdated := getUpdatedAt(model)
	setModelVersion(model, oldVersion+1)
	setUpdatedAt(model, time.Now())

	filter := andFilter(scope, buildVersionFilter(id, oldVersion))
	matched, err := replaceWithUnset(ctx, coll, schema, filter, model, unsetFields)
	if err != nil {
		setModelVersion(model, oldVersion)
		setUpdatedAt(model, oldUpdated)
		return err
	}
	if matched == 0 {
		setModelVersion(model, oldVersion)
		setUpdatedAt(model, oldUpdated)
		return checkUpdateConflict(ctx, coll, scope, id)
	}

	return nil
}

// mergeFromDB re-reads the document, computes a 3-way diff (base vs ours vs theirs),
// and applies non-conflicting changes from the caller onto the fresh DB state.
// Returns a *MergeConflictError if both sides modified the same fields.
// Fields the caller asked to unset count as removals on the caller's side.
// It returns the re-read state in map form, the base for a further round.
func mergeFromDB(ctx context.Context, coll *mongo.Collection, schema *Schema, model interface{}, base bson.M, id interface{}, scope interface{}, unset []string) (bson.M, error) {
	// Re-read the current document from the primary: a lagging secondary
	// could look older than a committed base and cause a false conflict.
	fresh := reflect.New(reflect.TypeOf(model).Elem()).Interface()
	primary := coll.Database().Collection(coll.Name(), options.Collection().SetReadPreference(readpref.Primary()))
	if err := primary.FindOne(ctx, andFilter(scope, bson.D{{Key: "_id", Value: id}})).Decode(fresh); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("goodm: failed to re-read document for merge: %w", err)
	}

	ours, err := toBsonMap(model)
	if err != nil {
		return nil, err
	}
	for _, f := range unset {
		delete(ours, f)
	}
	theirs, err := toBsonMap(fresh)
	if err != nil {
		return nil, err
	}
	// The re-read document is now the stored state the model builds on:
	// record it as the base so that, if this round's retry also conflicts
	// and retries run out, the model's version and base still agree and a
	// later WithRetry can merge from here.
	if !inTransaction(ctx) {
		if raw, err := bson.Marshal(fresh); err == nil {
			setModelBase(schema, model, raw)
		}
	}

	// A base newer than the stored document was recorded by a write that
	// never committed (a rolled-back transaction); merging against it would
	// take the caller's own rolled-back values for the stored state. Drop it.
	if baseVersion, theirVersion := versionIn(base), versionIn(theirs); baseVersion > theirVersion {
		setModelBase(schema, model, nil)
		return nil, ErrVersionConflict
	}

	ourChanges := diffFields(base, ours)
	theirChanges := diffFields(base, theirs)

	conflicts := fieldIntersection(ourChanges, theirChanges)
	if len(conflicts) > 0 {
		return nil, &MergeConflictError{Fields: conflicts}
	}

	// Build merged document: start with theirs, overlay our changes.
	merged := buildMergedDoc(theirs, ours, ourChanges)

	// Write the merged state back into the caller's model struct. Decoding
	// into the struct as it is would keep map keys and omitempty values the
	// merged document no longer has, so the BSON-mapped fields are zeroed
	// first; fields tagged "-", unexported fields (including the merge
	// base), and pointer identities are left alone.
	raw, err := bson.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("goodm: failed to marshal merged document: %w", err)
	}
	zeroBSONFields(reflect.ValueOf(model).Elem())
	if err := bson.Unmarshal(raw, model); err != nil {
		return nil, fmt.Errorf("goodm: failed to apply merged document: %w", err)
	}

	return theirs, nil
}

// zeroBSONFields sets every field the driver would encode to its zero value,
// recursing through inline embeds (keeping non-nil pointer embeds in place).
func zeroBSONFields(v reflect.Value) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		if name, _ := ParseBSONTag(sf.Tag.Get("bson")); name == "-" {
			continue
		}
		f := v.Field(i)
		if internal.IsInlineStruct(sf) {
			if f.Kind() == reflect.Pointer {
				if f.IsNil() {
					continue
				}
				f = f.Elem()
			}
			zeroBSONFields(f)
			continue
		}
		if f.CanSet() {
			f.Set(reflect.Zero(f.Type()))
		}
	}
}

// buildMergedDoc creates a merged document by starting with theirs and applying
// the caller's changed fields on top.
func buildMergedDoc(theirs, ours bson.M, ourChanges []string) bson.M {
	merged := make(bson.M, len(theirs))
	for k, v := range theirs {
		merged[k] = v
	}
	for _, field := range ourChanges {
		if val, exists := ours[field]; exists {
			merged[field] = val
		} else {
			delete(merged, field) // field was removed by caller (unset)
		}
	}
	return merged
}

// snapshotModel marshals a model to bson.M.
func snapshotModel(model interface{}) (bson.M, error) {
	return toBsonMap(model)
}

// toBsonMap marshals any value to a bson.M via round-trip through raw BSON.
// Nested documents decode as bson.M too, so two documents compare equal
// regardless of key order (a Go map field marshals in random order).
func toBsonMap(v interface{}) (bson.M, error) {
	raw, err := bson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("goodm: bson marshal failed: %w", err)
	}
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.DefaultDocumentM()
	var m bson.M
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("goodm: bson unmarshal failed: %w", err)
	}
	return m, nil
}

// diffFields returns the bson field names that differ between base and modified,
// excluding managed fields (_id, __v, timestamps) which are expected to change.
func diffFields(base, modified bson.M) []string {
	var changed []string
	for key, modVal := range modified {
		if managedFields[key] {
			continue
		}
		baseVal, exists := base[key]
		if !exists || !bsonValuesEqual(baseVal, modVal) {
			changed = append(changed, key)
		}
	}
	// Fields present in base but absent in modified (removed/unset).
	for key := range base {
		if managedFields[key] {
			continue
		}
		if _, exists := modified[key]; !exists {
			changed = append(changed, key)
		}
	}
	return changed
}

// versionIn returns the __v stored in a decoded document map, or -1.
func versionIn(doc bson.M) int64 {
	switch v := doc["__v"].(type) {
	case int32:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return -1
}

// bsonValuesEqual compares two decoded BSON values structurally, treating
// two NaN floats as equal (reflect.DeepEqual never would, which made a NaN
// field look changed on every merge).
func bsonValuesEqual(a, b interface{}) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || (math.IsNaN(x) && math.IsNaN(y)))
	case bson.M:
		y, ok := b.(bson.M)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !bsonValuesEqual(xv, yv) {
				return false
			}
		}
		return true
	case bson.A:
		y, ok := b.(bson.A)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !bsonValuesEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// fieldIntersection returns field names present in both slices.
func fieldIntersection(a, b []string) []string {
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	var result []string
	for _, s := range b {
		if set[s] {
			result = append(result, s)
		}
	}
	return result
}
