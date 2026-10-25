package goodm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ErrNoFullDocument is returned by ChangeEvent.Decode when the event carries
// no full document: a delete, an update watched without a full-document
// lookup, or an update whose document was deleted before the lookup ran.
var ErrNoFullDocument = errors.New("goodm: change event carries no full document")

// WatchOptions configures Watch.
type WatchOptions struct {
	DB *mongo.Database

	// Pipeline holds extra stages the server applies to each change event
	// before it is sent ($match, $project, $addFields, ...). Field paths refer
	// to the event, not the document: "operationType", "fullDocument.status",
	// "documentKey._id". Middleware may replace it through OpInfo.Pipeline.
	Pipeline []bson.D

	// FullDocument says what update events carry. Left zero it is
	// options.UpdateLookup, so every update event carries the document as it
	// is when the event is read (an update to a document that was deleted in
	// the meantime carries none). options.Default gives only the update
	// description; options.WhenAvailable and options.Required ask for the
	// post-image recorded with the write, which needs
	// changeStreamPreAndPostImages enabled on the collection (MongoDB 6.0+).
	FullDocument options.FullDocument

	// FullDocumentBeforeChange asks update, replace, and delete events for the
	// document as it was before the write (options.WhenAvailable or
	// options.Required). It needs changeStreamPreAndPostImages enabled on the
	// collection (MongoDB 6.0+).
	FullDocumentBeforeChange options.FullDocument

	// ResumeAfter resumes the stream after the event with this token
	// (ChangeStream.ResumeToken or ChangeEvent.ResumeToken). StartAfter does
	// the same but also works after an invalidate event. StartAtOperationTime
	// starts at a cluster time instead. At most one may be set.
	ResumeAfter          bson.Raw
	StartAfter           bson.Raw
	StartAtOperationTime *bson.Timestamp

	BatchSize          int32              // events per batch
	MaxAwaitTime       time.Duration      // how long the server waits for an event before returning an empty batch
	Collation          *options.Collation // collation for the pipeline's string comparisons
	ShowExpandedEvents bool               // include DDL events such as createIndexes and modify (MongoDB 6.0+)
}

// ChangeEvent is one change stream event. Fields the server did not send are
// zero: FullDocument is nil for deletes, UpdateDescription is nil for
// anything but updates, and WallTime is zero before MongoDB 6.0.
type ChangeEvent struct {
	ResumeToken              bson.Raw           `bson:"_id"`
	OperationType            string             `bson:"operationType"` // insert, update, replace, delete, drop, rename, dropDatabase, invalidate, ...
	ClusterTime              bson.Timestamp     `bson:"clusterTime"`
	WallTime                 time.Time          `bson:"wallTime"`
	Namespace                Namespace          `bson:"ns"`
	DocumentKey              bson.Raw           `bson:"documentKey"` // {_id: ...} plus the shard key on sharded collections
	FullDocument             bson.Raw           `bson:"fullDocument"`
	FullDocumentBeforeChange bson.Raw           `bson:"fullDocumentBeforeChange"`
	UpdateDescription        *UpdateDescription `bson:"updateDescription"`
	TxnNumber                int64              `bson:"txnNumber"`
	LSID                     bson.Raw           `bson:"lsid"`

	// Raw is the whole event as the server sent it, for fields not listed.
	Raw bson.Raw `bson:"-"`
}

// Namespace names the collection an event belongs to.
type Namespace struct {
	DB   string `bson:"db"`
	Coll string `bson:"coll"`
}

// UpdateDescription describes what an update changed.
type UpdateDescription struct {
	UpdatedFields   bson.Raw         `bson:"updatedFields"` // dotted paths to their new values
	RemovedFields   []string         `bson:"removedFields"`
	TruncatedArrays []TruncatedArray `bson:"truncatedArrays"`
}

// TruncatedArray records an array an update shortened.
type TruncatedArray struct {
	Field   string `bson:"field"`
	NewSize int32  `bson:"newSize"`
}

// Decode unmarshals the event's full document into model. The model becomes
// the merge base for a later Update with WithRetry, as after FindOne. It
// returns ErrNoFullDocument when the event carries none.
func (e *ChangeEvent) Decode(model interface{}) error {
	if len(e.FullDocument) == 0 {
		return ErrNoFullDocument
	}
	if err := bson.Unmarshal(e.FullDocument, model); err != nil {
		return fmt.Errorf("goodm: change event decode failed: %w", err)
	}
	if schema, ok := SchemaOf(model); ok {
		setModelBase(schema, model, e.FullDocument)
	}
	return nil
}

// DecodeBefore unmarshals the document as it was before the change into
// model. It returns ErrNoFullDocument when the event carries no pre-image.
func (e *ChangeEvent) DecodeBefore(model interface{}) error {
	if len(e.FullDocumentBeforeChange) == 0 {
		return ErrNoFullDocument
	}
	if err := bson.Unmarshal(e.FullDocumentBeforeChange, model); err != nil {
		return fmt.Errorf("goodm: change event decode failed: %w", err)
	}
	return nil
}

// DecodeID unmarshals the changed document's _id into id, a pointer to the
// model's ID type (for example *bson.ObjectID). Every document event has one.
func (e *ChangeEvent) DecodeID(id interface{}) error {
	if len(e.DocumentKey) == 0 {
		return errors.New("goodm: change event has no document key")
	}
	val, err := e.DocumentKey.LookupErr("_id")
	if err != nil {
		return fmt.Errorf("goodm: change event document key has no _id: %w", err)
	}
	if err := val.Unmarshal(id); err != nil {
		return fmt.Errorf("goodm: change event _id decode failed: %w", err)
	}
	return nil
}

// ObjectID returns the changed document's _id when it is an ObjectID.
func (e *ChangeEvent) ObjectID() (bson.ObjectID, bool) {
	if len(e.DocumentKey) == 0 {
		return bson.ObjectID{}, false
	}
	val, err := e.DocumentKey.LookupErr("_id")
	if err != nil {
		return bson.ObjectID{}, false
	}
	return val.ObjectIDOK()
}

// ChangeStream delivers the change events of a model's collection. Create
// one with Watch, read it with Next, and Close it when done.
type ChangeStream struct {
	cs     *mongo.ChangeStream
	schema *Schema
	event  *ChangeEvent
	err    error
}

// Watch opens a change stream on the model's collection. The model is used
// for schema lookup (e.g. &User{}). It needs a replica set or sharded
// cluster. Middleware runs under OpWatch and may replace OpInfo.Pipeline,
// for example to prepend a $match on "fullDocument.tenant"; delete events
// carry no full document, so such a scope drops them unless the stream asks
// for pre-images (FullDocumentBeforeChange).
//
//	stream, err := goodm.Watch(ctx, &Order{})
//	if err != nil { ... }
//	defer stream.Close(ctx)
//	for stream.Next(ctx) {
//	    ev := stream.Event()
//	    switch ev.OperationType {
//	    case "insert", "update", "replace":
//	        var order Order
//	        if err := ev.Decode(&order); err != nil { ... }
//	    case "delete":
//	        id, _ := ev.ObjectID()
//	    }
//	}
//	if err := stream.Err(); err != nil { ... }
func Watch(ctx context.Context, model interface{}, opts ...WatchOptions) (*ChangeStream, error) {
	schema, err := getSchemaForModel(model)
	if err != nil {
		return nil, err
	}
	var opt WatchOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if err := opt.validate(); err != nil {
		return nil, err
	}

	var stream *ChangeStream
	info := &OpInfo{
		Operation: OpWatch, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: model, Pipeline: opt.Pipeline,
	}
	err = runMiddleware(ctx, info, func(ctx context.Context) error {
		db, err := getDB(opt.DB)
		if err != nil {
			return err
		}
		pipeline := info.Pipeline
		if pipeline == nil {
			pipeline = []bson.D{}
		}
		coll := getCollection(db, schema)
		cs, err := coll.Watch(ctx, pipeline, opt.driverOptions())
		if err != nil {
			return fmt.Errorf("goodm: watch failed: %w", err)
		}
		stream = &ChangeStream{cs: cs, schema: schema}
		return nil
	})
	return stream, err
}

func (o WatchOptions) validate() error {
	n := 0
	if o.ResumeAfter != nil {
		n++
	}
	if o.StartAfter != nil {
		n++
	}
	if o.StartAtOperationTime != nil {
		n++
	}
	if n > 1 {
		return errors.New("goodm: WatchOptions: ResumeAfter, StartAfter, and StartAtOperationTime are mutually exclusive")
	}
	return nil
}

func (o WatchOptions) driverOptions() *options.ChangeStreamOptionsBuilder {
	b := options.ChangeStream()
	fd := o.FullDocument
	if fd == "" {
		fd = options.UpdateLookup
	}
	b.SetFullDocument(fd)
	if o.FullDocumentBeforeChange != "" {
		b.SetFullDocumentBeforeChange(o.FullDocumentBeforeChange)
	}
	if o.ResumeAfter != nil {
		b.SetResumeAfter(o.ResumeAfter)
	}
	if o.StartAfter != nil {
		b.SetStartAfter(o.StartAfter)
	}
	if o.StartAtOperationTime != nil {
		b.SetStartAtOperationTime(o.StartAtOperationTime)
	}
	if o.BatchSize > 0 {
		b.SetBatchSize(o.BatchSize)
	}
	if o.MaxAwaitTime > 0 {
		b.SetMaxAwaitTime(o.MaxAwaitTime)
	}
	if o.Collation != nil {
		b.SetCollation(*o.Collation)
	}
	if o.ShowExpandedEvents {
		b.SetShowExpandedEvents(true)
	}
	return b
}

// Next blocks until the next event is available and returns true, or returns
// false when ctx is done, the stream failed, or the server ended it (after an
// invalidate event, such as the collection being dropped). After a false
// return, Err reports the failure, or nil if the stream simply ended.
func (s *ChangeStream) Next(ctx context.Context) bool {
	return s.advance(ctx, false)
}

// TryNext returns the next event if one is already available and returns
// true; it returns false without blocking when none is, when ctx is done, or
// when the stream failed or ended. Check Err after a false return: when it
// is nil and ID is not zero the stream is still open and TryNext may be
// called again.
func (s *ChangeStream) TryNext(ctx context.Context) bool {
	return s.advance(ctx, true)
}

func (s *ChangeStream) advance(ctx context.Context, try bool) bool {
	if s.err != nil {
		return false
	}
	var ok bool
	if try {
		ok = s.cs.TryNext(ctx)
	} else {
		ok = s.cs.Next(ctx)
	}
	if !ok {
		if err := s.cs.Err(); err != nil {
			s.err = fmt.Errorf("goodm: change stream failed: %w", err)
		}
		return false
	}
	raw := copyRaw(s.cs.Current)
	ev := &ChangeEvent{Raw: raw}
	if err := bson.Unmarshal(raw, ev); err != nil {
		s.err = fmt.Errorf("goodm: change event decode failed: %w", err)
		return false
	}
	s.event = ev
	return true
}

// Event returns the event the last successful Next or TryNext produced.
func (s *ChangeStream) Event() *ChangeEvent { return s.event }

// Decode unmarshals the current event's full document into model, as
// ChangeEvent.Decode does.
func (s *ChangeStream) Decode(model interface{}) error {
	if s.event == nil {
		return errors.New("goodm: no current change event")
	}
	return s.event.Decode(model)
}

// Err returns the error that stopped the stream, or nil if it is open or
// ended normally.
func (s *ChangeStream) Err() error { return s.err }

// ResumeToken returns the token of the last event delivered (or the
// post-batch token when the last batch was empty), to pass as
// WatchOptions.ResumeAfter to a new stream. It is nil until the server has
// sent one.
func (s *ChangeStream) ResumeToken() bson.Raw { return s.cs.ResumeToken() }

// ID returns the server-side cursor ID, or zero once the server has closed
// the stream.
func (s *ChangeStream) ID() int64 { return s.cs.ID() }

// Close releases the stream's server-side cursor.
func (s *ChangeStream) Close(ctx context.Context) error { return s.cs.Close(ctx) }

// Driver returns the underlying driver change stream for options this
// wrapper does not expose. Reading events through it bypasses Event.
func (s *ChangeStream) Driver() *mongo.ChangeStream { return s.cs }

// Each calls fn for every event until ctx is done, fn returns an error, or
// the stream fails or ends. It returns fn's error, the stream's error (which
// wraps ctx.Err() when ctx ended it), or nil if the stream ended normally.
// It does not close the stream.
func (s *ChangeStream) Each(ctx context.Context, fn func(*ChangeEvent) error) error {
	for s.Next(ctx) {
		if err := fn(s.event); err != nil {
			return err
		}
	}
	return s.Err()
}
