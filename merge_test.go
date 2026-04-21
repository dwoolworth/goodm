package goodm

import (
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSnapshotModel(t *testing.T) {
	registerTestModels()
	defer unregisterTestModels()

	u := &testUser{
		Email: "alice@test.com",
		Name:  "Alice",
		Age:   25,
		Role:  "user",
	}
	u.ID = bson.NewObjectID()

	snap, err := snapshotModel(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if snap["email"] != "alice@test.com" {
		t.Fatalf("expected email alice@test.com, got %v", snap["email"])
	}
	if snap["name"] != "Alice" {
		t.Fatalf("expected name Alice, got %v", snap["name"])
	}

	// Snapshot should be independent — mutating the struct shouldn't affect it.
	u.Email = "bob@test.com"
	if snap["email"] != "alice@test.com" {
		t.Fatal("snapshot was mutated by struct change")
	}
}

func TestDiffFields_NoChanges(t *testing.T) {
	base := bson.M{"name": "Alice", "age": int32(25), "role": "user"}
	modified := bson.M{"name": "Alice", "age": int32(25), "role": "user"}

	changes := diffFields(base, modified)
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %v", changes)
	}
}

func TestDiffFields_ValueChanged(t *testing.T) {
	base := bson.M{"name": "Alice", "age": int32(25), "role": "user"}
	modified := bson.M{"name": "Alice", "age": int32(30), "role": "user"}

	changes := diffFields(base, modified)
	if len(changes) != 1 || changes[0] != "age" {
		t.Fatalf("expected [age], got %v", changes)
	}
}

func TestDiffFields_MultipleChanges(t *testing.T) {
	base := bson.M{"name": "Alice", "age": int32(25), "role": "user"}
	modified := bson.M{"name": "Bob", "age": int32(30), "role": "user"}

	changes := diffFields(base, modified)
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %v", changes)
	}
	changeSet := map[string]bool{}
	for _, c := range changes {
		changeSet[c] = true
	}
	if !changeSet["name"] || !changeSet["age"] {
		t.Fatalf("expected name and age changes, got %v", changes)
	}
}

func TestDiffFields_FieldAdded(t *testing.T) {
	base := bson.M{"name": "Alice"}
	modified := bson.M{"name": "Alice", "age": int32(25)}

	changes := diffFields(base, modified)
	if len(changes) != 1 || changes[0] != "age" {
		t.Fatalf("expected [age], got %v", changes)
	}
}

func TestDiffFields_FieldRemoved(t *testing.T) {
	base := bson.M{"name": "Alice", "age": int32(25)}
	modified := bson.M{"name": "Alice"}

	changes := diffFields(base, modified)
	if len(changes) != 1 || changes[0] != "age" {
		t.Fatalf("expected [age], got %v", changes)
	}
}

func TestDiffFields_SkipsManagedFields(t *testing.T) {
	base := bson.M{"name": "Alice", "__v": int32(1), "updated_at": "old"}
	modified := bson.M{"name": "Alice", "__v": int32(2), "updated_at": "new"}

	changes := diffFields(base, modified)
	if len(changes) != 0 {
		t.Fatalf("expected no changes (managed fields skipped), got %v", changes)
	}
}

func TestFieldIntersection_NoOverlap(t *testing.T) {
	a := []string{"name", "age"}
	b := []string{"role", "email"}

	result := fieldIntersection(a, b)
	if len(result) != 0 {
		t.Fatalf("expected empty intersection, got %v", result)
	}
}

func TestFieldIntersection_WithOverlap(t *testing.T) {
	a := []string{"name", "age", "role"}
	b := []string{"role", "email"}

	result := fieldIntersection(a, b)
	if len(result) != 1 || result[0] != "role" {
		t.Fatalf("expected [role], got %v", result)
	}
}

func TestFieldIntersection_FullOverlap(t *testing.T) {
	a := []string{"name", "age"}
	b := []string{"age", "name"}

	result := fieldIntersection(a, b)
	if len(result) != 2 {
		t.Fatalf("expected 2 conflicts, got %v", result)
	}
}

func TestFieldIntersection_EmptyInputs(t *testing.T) {
	result := fieldIntersection(nil, nil)
	if len(result) != 0 {
		t.Fatalf("expected empty, got %v", result)
	}

	result = fieldIntersection([]string{"a"}, nil)
	if len(result) != 0 {
		t.Fatalf("expected empty, got %v", result)
	}
}

func TestBuildMergedDoc_DisjointChanges(t *testing.T) {
	theirs := bson.M{"name": "Alice", "age": int32(30), "role": "admin"}
	ours := bson.M{"name": "Alice", "age": int32(25), "role": "user"}
	ourChanges := []string{"age"} // we changed age from 25→25 (theirs changed to 30), but this tests the apply

	merged := buildMergedDoc(theirs, ours, ourChanges)

	// Our age should override theirs
	if merged["age"] != int32(25) {
		t.Fatalf("expected age 25, got %v", merged["age"])
	}
	// Their role should be preserved
	if merged["role"] != "admin" {
		t.Fatalf("expected role admin, got %v", merged["role"])
	}
	// Their name should be preserved
	if merged["name"] != "Alice" {
		t.Fatalf("expected name Alice, got %v", merged["name"])
	}
}

func TestBuildMergedDoc_FieldRemovedByUs(t *testing.T) {
	theirs := bson.M{"name": "Alice", "agent_id": "abc123"}
	ours := bson.M{"name": "Alice"} // agent_id removed
	ourChanges := []string{"agent_id"}

	merged := buildMergedDoc(theirs, ours, ourChanges)

	if _, exists := merged["agent_id"]; exists {
		t.Fatal("expected agent_id to be removed from merged doc")
	}
	if merged["name"] != "Alice" {
		t.Fatalf("expected name Alice, got %v", merged["name"])
	}
}

func TestWithRetry_Constructor(t *testing.T) {
	opts := WithRetry(3)
	if opts.MaxRetries != 3 {
		t.Fatalf("expected MaxRetries 3, got %d", opts.MaxRetries)
	}
}

func TestMergeConflictError(t *testing.T) {
	err := &MergeConflictError{Fields: []string{"status", "result"}}

	if err.Error() != "goodm: merge conflict on fields: status, result" {
		t.Fatalf("unexpected error message: %s", err.Error())
	}

	// Should be detectable with errors.As
	var mergeErr *MergeConflictError
	if !errors.As(err, &mergeErr) {
		t.Fatal("expected errors.As to match MergeConflictError")
	}
	if len(mergeErr.Fields) != 2 {
		t.Fatalf("expected 2 conflicting fields, got %d", len(mergeErr.Fields))
	}
}

// TestEndToEndMergeScenario tests the full diff→intersect→merge flow using
// bson.M values that simulate the supervisor/worker conflict from issue #5.
func TestEndToEndMergeScenario(t *testing.T) {
	// Base state when both read the document.
	base := bson.M{
		"_id":            "task1",
		"__v":            int32(10),
		"updated_at":     "t0",
		"step":           int32(4),
		"tokens_used":    int32(8000),
		"last_checked_at": "1pm",
		"status":         "running",
	}

	// Worker changed step and tokens_used.
	ours := bson.M{
		"_id":            "task1",
		"__v":            int32(10),
		"updated_at":     "t0",
		"step":           int32(5),
		"tokens_used":    int32(12000),
		"last_checked_at": "1pm",
		"status":         "running",
	}

	// Supervisor changed last_checked_at.
	theirs := bson.M{
		"_id":            "task1",
		"__v":            int32(11),
		"updated_at":     "t1",
		"step":           int32(4),
		"tokens_used":    int32(8000),
		"last_checked_at": "2pm",
		"status":         "running",
	}

	ourChanges := diffFields(base, ours)
	theirChanges := diffFields(base, theirs)

	// Our changes should be step and tokens_used.
	ourSet := map[string]bool{}
	for _, c := range ourChanges {
		ourSet[c] = true
	}
	if !ourSet["step"] || !ourSet["tokens_used"] || len(ourChanges) != 2 {
		t.Fatalf("expected [step, tokens_used], got %v", ourChanges)
	}

	// Their changes should be last_checked_at.
	if len(theirChanges) != 1 || theirChanges[0] != "last_checked_at" {
		t.Fatalf("expected [last_checked_at], got %v", theirChanges)
	}

	// No conflicts — disjoint fields.
	conflicts := fieldIntersection(ourChanges, theirChanges)
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %v", conflicts)
	}

	// Merge should have our step/tokens + their last_checked_at.
	merged := buildMergedDoc(theirs, ours, ourChanges)
	if merged["step"] != int32(5) {
		t.Fatalf("expected step 5, got %v", merged["step"])
	}
	if merged["tokens_used"] != int32(12000) {
		t.Fatalf("expected tokens_used 12000, got %v", merged["tokens_used"])
	}
	if merged["last_checked_at"] != "2pm" {
		t.Fatalf("expected last_checked_at 2pm, got %v", merged["last_checked_at"])
	}
}

// TestConflictingMergeScenario verifies that overlapping field changes are detected.
func TestConflictingMergeScenario(t *testing.T) {
	base := bson.M{"status": "running", "step": int32(4)}

	ours := bson.M{"status": "completed", "step": int32(5)}  // we changed both
	theirs := bson.M{"status": "failed", "step": int32(4)}   // they changed status

	ourChanges := diffFields(base, ours)
	theirChanges := diffFields(base, theirs)

	conflicts := fieldIntersection(ourChanges, theirChanges)
	if len(conflicts) != 1 || conflicts[0] != "status" {
		t.Fatalf("expected conflict on [status], got %v", conflicts)
	}
}
