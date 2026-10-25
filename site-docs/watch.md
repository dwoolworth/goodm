# Change Streams

`Watch` opens a MongoDB change stream on a model's collection and delivers every insert, update, replace, and delete as a `ChangeEvent`. Change streams need a replica set or sharded cluster; on a standalone server `Watch` returns the server's error.

## Watching a Collection

```go
stream, err := goodm.Watch(ctx, &Order{})
if err != nil {
    return err
}
defer stream.Close(ctx)

for stream.Next(ctx) {
    ev := stream.Event()
    switch ev.OperationType {
    case "insert", "update", "replace":
        var order Order
        if err := ev.Decode(&order); err != nil {
            return err
        }
        fmt.Println("changed:", order.ID, order.Status)
    case "delete":
        id, _ := ev.ObjectID()
        fmt.Println("deleted:", id)
    }
}
if err := stream.Err(); err != nil {
    return err // includes ctx.Err() when the context ended the stream
}
```

`Next` blocks until an event arrives, the context ends, the stream fails, or the server ends it. After `Next` returns false, `Err` reports the failure, or nil when the stream ended normally (see [Invalidation](#invalidation)). A stream that has failed stays failed; open a new one, resuming from the last token.

`TryNext` returns false immediately when no event is waiting; check `Err` and `ID` (zero once the server has closed the stream) to tell "nothing yet" from "ended".

`Each` runs a handler for every event:

```go
err := stream.Each(ctx, func(ev *goodm.ChangeEvent) error {
    return handle(ev)
})
```

It returns the handler's error, the stream's error, or nil when the stream ended normally. It does not close the stream.

## Events

```go
type ChangeEvent struct {
    ResumeToken              bson.Raw           // pass to WatchOptions.ResumeAfter or StartAfter
    OperationType            string             // insert, update, replace, delete, drop, rename, dropDatabase, invalidate, ...
    ClusterTime              bson.Timestamp
    WallTime                 time.Time          // MongoDB 6.0+
    Namespace                goodm.Namespace    // DB and Coll
    DocumentKey              bson.Raw           // {_id: ...}, plus the shard key on sharded collections
    FullDocument             bson.Raw
    FullDocumentBeforeChange bson.Raw
    UpdateDescription        *goodm.UpdateDescription // UpdatedFields, RemovedFields, TruncatedArrays
    TxnNumber                int64
    LSID                     bson.Raw
    Raw                      bson.Raw           // the whole event
}
```

Fields the server did not send are zero.

- `Decode(&model)` unmarshals `FullDocument` into a model. The model becomes the merge base for a later `Update` with `WithRetry`, exactly as after `FindOne`, so a worker that reacts to an event can save its own changes without clobbering a concurrent writer. It returns `goodm.ErrNoFullDocument` for a delete, or for an update whose document was deleted before the lookup ran.
- `DecodeBefore(&model)` unmarshals the pre-image, when the stream asked for one.
- `DecodeID(&id)` and `ObjectID()` read the `_id` from the document key. Every document event has one.

Which operations produce which event types:

| goodm call | Event |
|------------|-------|
| `Create`, `CreateMany`, `Bulk.InsertOne` | `insert` |
| `Update`, `SaveMany`, `FindOneAndReplace`, `Bulk.ReplaceOne` | `replace` |
| `UpdateFields`, `UpdateOne`, `UpdateMany`, `FindOneAndUpdate`, `Bulk.UpdateOne/UpdateMany` | `update` |
| `Delete`, `DeleteOne`, `DeleteMany`, `FindOneAndDelete`, `Bulk.DeleteOne/DeleteMany` | `delete` |

`Update` replaces the whole document, so its events are `replace` and carry no `UpdateDescription`. `UpdateFields` events do.

## Full Documents and Pre-Images

By default `Watch` asks for `options.UpdateLookup`: an `update` event carries the document as it is when the event is read, which may already include later writes. Insert and replace events always carry the written document. To receive only the update description, set `FullDocument: options.Default`.

MongoDB 6.0+ can record the document before and after each write with the collection's `changeStreamPreAndPostImages` setting:

```go
db.RunCommand(ctx, bson.D{
    {Key: "collMod", Value: "orders"},
    {Key: "changeStreamPreAndPostImages", Value: bson.D{{Key: "enabled", Value: true}}},
})
```

Then ask for them:

```go
stream, err := goodm.Watch(ctx, &Order{}, goodm.WatchOptions{
    FullDocument:             options.WhenAvailable, // the post-image recorded with the write
    FullDocumentBeforeChange: options.WhenAvailable, // the pre-image, also on delete
})
```

`options.Required` makes the server fail the event instead of omitting an image it did not record.

## Resuming

Each event carries a `ResumeToken`, and `stream.ResumeToken()` returns the latest one the server sent (including after an empty batch). Persist it and pass it back to continue where you left off; events are replayed from the oplog, so a restarted worker misses nothing the oplog still holds:

```go
stream, err := goodm.Watch(ctx, &Order{}, goodm.WatchOptions{ResumeAfter: savedToken})
```

`StartAfter` does the same and also works after an `invalidate` event. `StartAtOperationTime` starts from a cluster time instead. At most one of the three may be set.

The driver resumes transparently after a transient network error or a failover, so `Next` keeps delivering across those.

## Filtering

`WatchOptions.Pipeline` adds stages the server applies to each event before sending it. Field paths refer to the event, not the document:

```go
stream, err := goodm.Watch(ctx, &Order{}, goodm.WatchOptions{
    Pipeline: []bson.D{{{Key: "$match", Value: bson.D{
        {Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert", "update"}}}},
        {Key: "fullDocument.status", Value: "paid"},
    }}}},
})
```

`Watch` runs middleware under `OpWatch`, and middleware may replace `op.Pipeline` just as for `OpAggregate`. A tenant scope on `fullDocument.tenant` filters out delete events, because they carry no full document; match on `documentKey` or use pre-images if deletes must be scoped too.

## Options

```go
type WatchOptions struct {
    DB                       *mongo.Database
    Pipeline                 []bson.D
    FullDocument             options.FullDocument // zero = options.UpdateLookup
    FullDocumentBeforeChange options.FullDocument
    ResumeAfter              bson.Raw
    StartAfter               bson.Raw
    StartAtOperationTime     *bson.Timestamp
    BatchSize                int32
    MaxAwaitTime             time.Duration
    Collation                *options.Collation
    ShowExpandedEvents       bool // DDL events such as createIndexes (MongoDB 6.0+)
}
```

The stream uses the model's `CollectionOptions` read preference and read concern. `stream.Driver()` returns the underlying `*mongo.ChangeStream` for anything the wrapper does not expose.

## Invalidation

Dropping or renaming the collection, or dropping the database, sends a `drop`, `rename`, or `dropDatabase` event followed by `invalidate`, after which the server closes the stream: `Next` returns false with a nil `Err`. To keep watching the new collection, open a new stream with `StartAfter` set to the invalidate event's token.
