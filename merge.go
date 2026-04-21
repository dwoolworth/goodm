package goodm

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// saveWithRetry attempts a versioned save, optionally retrying with a 3-way
// field-level merge when a version conflict occurs. Without retries it still
// refreshes the model's version on conflict to prevent cascading failures.
func saveWithRetry(ctx context.Context, coll *mongo.Collection, model interface{}, opt UpdateOptions, id bson.ObjectID) error {
	var base bson.M
	if opt.MaxRetries > 0 {
		var err error
		base, err = snapshotModel(model)
		if err != nil {
			return err
		}
	}

	for attempt := 0; ; attempt++ {
		err := attemptSave(ctx, coll, model, opt.Unset, id)
		if err == nil {
			return nil
		}
		if err != ErrVersionConflict {
			return err
		}

		// Version conflict — can we retry with merge?
		if base == nil || attempt >= opt.MaxRetries {
			// No retry: refresh version so next caller Update() can succeed.
			refreshModelVersion(ctx, coll, model, id)
			return ErrVersionConflict
		}

		// 3-way merge: re-read DB state, detect conflicts, apply disjoint changes.
		if err := mergeFromDB(ctx, coll, model, base, id); err != nil {
			return err
		}
	}
}

// attemptSave performs a single versioned replace. Returns ErrVersionConflict
// if the version filter did not match, or ErrNotFound if the document is gone.
func attemptSave(ctx context.Context, coll *mongo.Collection, model interface{}, unsetFields []string, id bson.ObjectID) error {
	oldVersion, _ := getModelVersion(model)
	setModelVersion(model, oldVersion+1)
	setUpdatedAt(model, time.Now())

	filter := buildVersionFilter(id, oldVersion)
	matched, err := replaceWithUnset(ctx, coll, filter, model, unsetFields)
	if err != nil {
		setModelVersion(model, oldVersion)
		return fmt.Errorf("goodm: update failed: %w", err)
	}
	if matched == 0 {
		setModelVersion(model, oldVersion)
		return checkUpdateConflict(ctx, coll, id)
	}

	return nil
}

// mergeFromDB re-reads the document, computes a 3-way diff (base vs ours vs theirs),
// and applies non-conflicting changes from the caller onto the fresh DB state.
// Returns a *MergeConflictError if both sides modified the same fields.
func mergeFromDB(ctx context.Context, coll *mongo.Collection, model interface{}, base bson.M, id bson.ObjectID) error {
	// Re-read the current document from the database.
	fresh := reflect.New(reflect.TypeOf(model).Elem()).Interface()
	if err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(fresh); err != nil {
		if err == mongo.ErrNoDocuments {
			return ErrNotFound
		}
		return fmt.Errorf("goodm: failed to re-read document for merge: %w", err)
	}

	ours, err := toBsonMap(model)
	if err != nil {
		return err
	}
	theirs, err := toBsonMap(fresh)
	if err != nil {
		return err
	}

	ourChanges := diffFields(base, ours)
	theirChanges := diffFields(base, theirs)

	conflicts := fieldIntersection(ourChanges, theirChanges)
	if len(conflicts) > 0 {
		return &MergeConflictError{Fields: conflicts}
	}

	// Build merged document: start with theirs, overlay our changes.
	merged := buildMergedDoc(theirs, ours, ourChanges)

	// Write the merged state back into the caller's model struct.
	raw, err := bson.Marshal(merged)
	if err != nil {
		return fmt.Errorf("goodm: failed to marshal merged document: %w", err)
	}
	if err := bson.Unmarshal(raw, model); err != nil {
		return fmt.Errorf("goodm: failed to apply merged document: %w", err)
	}

	return nil
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

// refreshModelVersion does a best-effort read of the document's current version
// and updates the model struct so the next Update() call won't cascade-fail.
func refreshModelVersion(ctx context.Context, coll *mongo.Collection, model interface{}, id bson.ObjectID) {
	var doc struct {
		Version int `bson:"__v"`
	}
	err := coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(bson.D{{Key: "__v", Value: 1}})).Decode(&doc)
	if err == nil {
		setModelVersion(model, doc.Version)
	}
}

// snapshotModel marshals a model to bson.M, capturing the "base" state before
// any modifications. Used as the reference point for 3-way merge.
func snapshotModel(model interface{}) (bson.M, error) {
	return toBsonMap(model)
}

// toBsonMap marshals any value to a bson.M via round-trip through raw BSON.
func toBsonMap(v interface{}) (bson.M, error) {
	raw, err := bson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("goodm: bson marshal failed: %w", err)
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
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
		if !exists || !reflect.DeepEqual(baseVal, modVal) {
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
