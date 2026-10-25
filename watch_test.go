package goodm

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// nextEvent reads one event with a bounded wait.
func nextEvent(t *testing.T, s *ChangeStream) *ChangeEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.Next(ctx) {
		t.Fatalf("expected an event, stream ended: err=%v", s.Err())
	}
	return s.Event()
}

func openStream(t *testing.T, ctx context.Context, model interface{}, opts ...WatchOptions) *ChangeStream {
	t.Helper()
	s, err := Watch(ctx, model, opts...)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestWatchLifecycleEvents(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	s := openStream(t, ctx, &testUser{})

	u := &testUser{Email: "w@x.com", Name: "W", Age: 1}
	if err := Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, s)
	if ev.OperationType != "insert" {
		t.Fatalf("op = %q, want insert", ev.OperationType)
	}
	if ev.Namespace.Coll != "test_users" {
		t.Errorf("ns.coll = %q", ev.Namespace.Coll)
	}
	if id, ok := ev.ObjectID(); !ok || id != u.ID {
		t.Errorf("ObjectID = %v %v, want %v", id, ok, u.ID)
	}
	var id bson.ObjectID
	if err := ev.DecodeID(&id); err != nil || id != u.ID {
		t.Errorf("DecodeID = %v %v", id, err)
	}
	if ev.ResumeToken == nil || s.ResumeToken() == nil {
		t.Error("expected resume tokens")
	}
	if ev.ClusterTime.T == 0 {
		t.Error("expected a cluster time")
	}
	var got testUser
	if err := ev.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Email != "w@x.com" || got.ID != u.ID || got.Version != 0 {
		t.Errorf("decoded %+v", got)
	}

	// Update replaces the document.
	u.Age = 2
	if err := Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, s)
	if ev.OperationType != "replace" {
		t.Fatalf("op = %q, want replace", ev.OperationType)
	}
	if err := s.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Age != 2 || got.Version != 1 {
		t.Errorf("decoded %+v", got)
	}

	// UpdateFields issues an update with a description; the default
	// UpdateLookup still carries the full document.
	if err := UpdateFields(ctx, u, bson.M{"age": 3}); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, s)
	if ev.OperationType != "update" {
		t.Fatalf("op = %q, want update", ev.OperationType)
	}
	if ev.UpdateDescription == nil {
		t.Fatal("expected an update description")
	}
	if v, err := ev.UpdateDescription.UpdatedFields.LookupErr("age"); err != nil || v.AsInt64() != 3 {
		t.Errorf("updatedFields.age = %v %v", v, err)
	}
	if err := ev.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Age != 3 || got.Version != 2 {
		t.Errorf("decoded %+v", got)
	}

	// Delete carries only the key.
	if err := Delete(ctx, u); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, s)
	if ev.OperationType != "delete" {
		t.Fatalf("op = %q, want delete", ev.OperationType)
	}
	if err := ev.Decode(&got); !errors.Is(err, ErrNoFullDocument) {
		t.Errorf("Decode on delete = %v, want ErrNoFullDocument", err)
	}
	if gotID, ok := ev.ObjectID(); !ok || gotID != u.ID {
		t.Errorf("delete key = %v %v", gotID, ok)
	}
}

func TestWatchDecodeSetsMergeBase(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	s := openStream(t, ctx, &testUser{})
	u := &testUser{Email: "base@x.com", Name: "B", Age: 1}
	if err := Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, s)

	// A model decoded from the event can merge with WithRetry: another
	// writer changes role, this copy changes age, both survive.
	var mine testUser
	if err := ev.Decode(&mine); err != nil {
		t.Fatal(err)
	}
	other := &testUser{}
	if err := FindByID(ctx, u.ID, other); err != nil {
		t.Fatal(err)
	}
	other.Role = "admin"
	if err := Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	mine.Age = 42
	if err := Update(ctx, &mine, WithRetry(1)); err != nil {
		t.Fatalf("WithRetry merge failed: %v", err)
	}
	var stored testUser
	if err := FindByID(ctx, u.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Age != 42 || stored.Role != "admin" || stored.Version != 2 {
		t.Errorf("stored %+v", stored)
	}

	// Without a base (a plain struct) the same conflict is reported.
	plain := &testUser{Model: Model{ID: u.ID, Version: 0}, Email: u.Email, Name: u.Name, Age: 7}
	if err := Update(ctx, plain, WithRetry(1)); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("plain update = %v, want ErrVersionConflict", err)
	}
}

func TestWatchResumeAfter(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	s := openStream(t, ctx, &testUser{})
	first := &testUser{Email: "r1@x.com", Name: "R1"}
	second := &testUser{Email: "r2@x.com", Name: "R2"}
	if err := Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, s)
	if id, _ := ev.ObjectID(); id != first.ID {
		t.Fatalf("first event is %v", id)
	}
	token := s.ResumeToken()
	_ = s.Close(ctx)

	resumed := openStream(t, ctx, &testUser{}, WatchOptions{ResumeAfter: token})
	ev = nextEvent(t, resumed)
	if id, _ := ev.ObjectID(); id != second.ID || ev.OperationType != "insert" {
		t.Errorf("resumed event = %s %v, want insert of %v", ev.OperationType, id, second.ID)
	}

	// StartAfter with the event's own token works the same way.
	again := openStream(t, ctx, &testUser{}, WatchOptions{StartAfter: ev.ResumeToken})
	third := &testUser{Email: "r3@x.com", Name: "R3"}
	if err := Create(ctx, third); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, again)
	if id, _ := ev.ObjectID(); id != third.ID {
		t.Errorf("StartAfter event = %v, want %v", id, third.ID)
	}
}

func TestWatchPipelineAndMiddleware(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	// The caller's pipeline keeps only deletes and inserts of admins...
	callerPipeline := []bson.D{{{Key: "$match", Value: bson.D{
		{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert", "delete"}}}},
	}}}}
	// ...and middleware narrows inserts to admins and sees OpWatch.
	var seen []OpType
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		seen = append(seen, op.Operation)
		if op.Operation == OpWatch {
			if op.Collection != "test_users" || op.ModelName != "testUser" || op.Filter != nil {
				t.Errorf("OpInfo = %+v", op)
			}
			if len(op.Pipeline) != 1 {
				t.Errorf("middleware saw pipeline %v", op.Pipeline)
			}
			scope := bson.D{{Key: "$match", Value: bson.D{{Key: "$or", Value: bson.A{
				bson.D{{Key: "operationType", Value: "delete"}},
				bson.D{{Key: "fullDocument.role", Value: "admin"}},
			}}}}}
			op.Pipeline = append(append([]bson.D{}, op.Pipeline...), scope)
		}
		return next(ctx)
	})

	s := openStream(t, ctx, &testUser{}, WatchOptions{Pipeline: callerPipeline})
	if len(seen) != 1 || seen[0] != OpWatch {
		t.Fatalf("middleware ops = %v", seen)
	}

	user := &testUser{Email: "u@x.com", Name: "U", Role: "user"}
	admin := &testUser{Email: "a@x.com", Name: "A", Role: "admin"}
	if err := Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, admin); err != nil {
		t.Fatal(err)
	}
	admin.Age = 9
	if err := Update(ctx, admin); err != nil { // replace: filtered by the caller
		t.Fatal(err)
	}
	if err := Delete(ctx, user); err != nil {
		t.Fatal(err)
	}

	ev := nextEvent(t, s)
	if id, _ := ev.ObjectID(); ev.OperationType != "insert" || id != admin.ID {
		t.Errorf("first = %s %v, want insert of admin %v", ev.OperationType, id, admin.ID)
	}
	ev = nextEvent(t, s)
	if id, _ := ev.ObjectID(); ev.OperationType != "delete" || id != user.ID {
		t.Errorf("second = %s %v, want delete of user %v", ev.OperationType, id, user.ID)
	}
}

func TestWatchTryNextAndEach(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	s := openStream(t, ctx, &testUser{}, WatchOptions{MaxAwaitTime: 50 * time.Millisecond})
	if s.TryNext(ctx) {
		t.Fatal("TryNext returned an event on an idle stream")
	}
	if s.Err() != nil || s.ID() == 0 {
		t.Fatalf("idle stream: err=%v id=%d", s.Err(), s.ID())
	}

	u := &testUser{Email: "t@x.com", Name: "T"}
	if err := Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for !s.TryNext(deadline) {
		if s.Err() != nil || deadline.Err() != nil {
			t.Fatalf("TryNext never saw the insert: %v", s.Err())
		}
	}
	if s.Event().OperationType != "insert" {
		t.Errorf("op = %s", s.Event().OperationType)
	}

	// Each stops at the handler's error and leaves the stream usable.
	for i := 0; i < 3; i++ {
		if err := Create(ctx, &testUser{Email: "e" + string(rune('a'+i)) + "@x.com", Name: "E"}); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("stop")
	n := 0
	err := s.Each(ctx, func(ev *ChangeEvent) error {
		n++
		if n == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || n != 2 {
		t.Fatalf("Each = %v after %d events", err, n)
	}
	if s.Err() != nil {
		t.Fatalf("stream err after handler stop: %v", s.Err())
	}

	// Each returns the stream's error (wrapping the context's) when the
	// context ends it.
	short, cancelShort := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelShort()
	err = s.Each(short, func(*ChangeEvent) error { n++; return nil })
	if n != 3 {
		t.Errorf("saw %d events, want 3", n)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Each after deadline = %v", err)
	}
	if !errors.Is(s.Err(), context.DeadlineExceeded) || s.Next(ctx) {
		t.Errorf("a failed stream must stay failed: err=%v", s.Err())
	}
}

func TestWatchInvalidateEndsStream(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	if err := Create(ctx, &testUser{Email: "i@x.com", Name: "I"}); err != nil {
		t.Fatal(err)
	}
	s := openStream(t, ctx, &testUser{})
	if err := db.Collection("test_users").Drop(ctx); err != nil {
		t.Fatal(err)
	}
	var ops []string
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for s.Next(deadline) {
		ops = append(ops, s.Event().OperationType)
	}
	if s.Err() != nil {
		t.Fatalf("stream ended with error: %v", s.Err())
	}
	if len(ops) != 2 || ops[0] != "drop" || ops[1] != "invalidate" {
		t.Errorf("ops = %v, want [drop invalidate]", ops)
	}
	if s.ID() != 0 {
		t.Errorf("stream still open after invalidate")
	}
}

func TestWatchPreImages(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	// Pre-images need the collection flag (MongoDB 6.0+).
	if err := db.CreateCollection(ctx, "test_users"); err != nil {
		t.Fatal(err)
	}
	cmd := bson.D{{Key: "collMod", Value: "test_users"}, {Key: "changeStreamPreAndPostImages", Value: bson.D{{Key: "enabled", Value: true}}}}
	if err := db.RunCommand(ctx, cmd).Err(); err != nil {
		t.Skipf("pre-images not supported: %v", err)
	}

	s := openStream(t, ctx, &testUser{}, WatchOptions{
		FullDocument:             options.WhenAvailable,
		FullDocumentBeforeChange: options.WhenAvailable,
	})
	u := &testUser{Email: "p@x.com", Name: "P", Age: 1}
	if err := Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, s)
	if err := ev.DecodeBefore(&testUser{}); !errors.Is(err, ErrNoFullDocument) {
		t.Errorf("insert pre-image = %v", err)
	}
	if err := UpdateFields(ctx, u, bson.M{"age": 2}); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, s)
	var before, after testUser
	if err := ev.DecodeBefore(&before); err != nil {
		t.Fatal(err)
	}
	if err := ev.Decode(&after); err != nil {
		t.Fatal(err)
	}
	if before.Age != 1 || after.Age != 2 {
		t.Errorf("before %d after %d", before.Age, after.Age)
	}
	if err := Delete(ctx, u); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, s)
	if err := ev.DecodeBefore(&before); err != nil || before.Age != 2 {
		t.Errorf("delete pre-image: %+v %v", before, err)
	}
}

func TestWatchErrors(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	type unregistered struct {
		Model `bson:",inline"`
	}
	if _, err := Watch(ctx, &unregistered{}); err == nil {
		t.Error("expected an error for an unregistered model")
	}
	tok := bson.Raw{5, 0, 0, 0, 0}
	if _, err := Watch(ctx, &testUser{}, WatchOptions{ResumeAfter: tok, StartAfter: tok}); err == nil {
		t.Error("expected an error for two resume options")
	}
	if _, err := Watch(ctx, &testUser{}, WatchOptions{ResumeAfter: tok}); err == nil {
		t.Error("expected an error for a bogus resume token")
	}

	s := openStream(t, ctx, &testUser{})
	if err := s.Decode(&testUser{}); err == nil {
		t.Error("Decode before Next must fail")
	}
	var ev ChangeEvent
	if err := ev.DecodeID(new(bson.ObjectID)); err == nil {
		t.Error("DecodeID on an empty event must fail")
	}
	if _, ok := ev.ObjectID(); ok {
		t.Error("ObjectID on an empty event must be false")
	}
}
