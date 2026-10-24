# Aggregation Pipeline

goodm provides a fluent builder for MongoDB aggregation pipelines. Chain stages together and execute against a collection.

## Creating a Pipeline

```go
pipe := goodm.NewPipeline(&User{})
```

The model parameter determines the collection. Options override the database and pass aggregation settings through:

```go
pipe := goodm.NewPipeline(&User{}, goodm.PipelineOptions{
    DB:           otherDB,
    AllowDiskUse: true,      // let big $group/$sort stages spill to disk
    Collation:    &options.Collation{Locale: "en", Strength: 2},
    Hint:         "role_1",
    BatchSize:    500,
})
```

`Execute` and `Cursor` run [middleware](middleware.md) under `OpAggregate`; middleware may prepend or replace stages via `op.Pipeline`.

## Stages

### Match

Filter documents (equivalent to `find`):

```go
pipe.Match(bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}})
```

### Group

Aggregate by grouping:

```go
pipe.Group(bson.D{
    {Key: "_id", Value: "$role"},
    {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
    {Key: "avgAge", Value: bson.D{{Key: "$avg", Value: "$age"}}},
})
```

### Sort

Order results:

```go
pipe.Sort(bson.D{{Key: "count", Value: -1}}) // descending
```

### Project

Reshape documents:

```go
pipe.Project(bson.D{
    {Key: "email", Value: 1},
    {Key: "name", Value: 1},
    {Key: "_id", Value: 0},
})
```

### Limit / Skip

Pagination:

```go
pipe.Skip(20).Limit(10)
```

### Unwind

Deconstruct an array field (auto-prefixed with `$`):

```go
pipe.Unwind("tags")
// Produces: {$unwind: "$tags"}

pipe.UnwindPreserve("tags")
// Produces: {$unwind: {path: "$tags", preserveNullAndEmptyArrays: true}}
```

### Lookup

Left outer join:

```go
pipe.Lookup("orders", "user_id", "_id", "user_orders")
// Joins the "orders" collection where orders.user_id == users._id
// Results stored in "user_orders" array
```

With a sub-pipeline and `let` variables for conditions beyond field equality:

```go
pipe.LookupPipeline("orders",
    bson.D{{Key: "uid", Value: "$_id"}},
    []bson.D{
        {{Key: "$match", Value: bson.D{{Key: "$expr", Value: bson.D{{Key: "$and", Value: bson.A{
            bson.D{{Key: "$eq", Value: bson.A{"$user_id", "$$uid"}}},
            bson.D{{Key: "$gt", Value: bson.A{"$total", 100}}},
        }}}}}}},
    },
    "big_orders")
```

### Set / Unset / ReplaceRoot / Facet / Sample

```go
pipe.Set(bson.D{{Key: "year", Value: bson.D{{Key: "$year", Value: "$created_at"}}}})
pipe.Unset("password", "internal")
pipe.ReplaceRoot("$profile")
pipe.Facet(map[string][]bson.D{
    "byRole": {{{Key: "$sortByCount", Value: "$role"}}},
    "total":  {{{Key: "$count", Value: "n"}}},
})
pipe.Sample(10)
```

### AddFields

Add computed fields:

```go
pipe.AddFields(bson.D{
    {Key: "fullName", Value: bson.D{{Key: "$concat", Value: bson.A{"$first", " ", "$last"}}}},
})
```

### Count

Count documents at the current stage:

```go
pipe.Count("total")
```

### Stage (Raw)

Add any stage not covered by the builder:

```go
pipe.Stage(bson.D{{Key: "$sample", Value: bson.D{{Key: "size", Value: 5}}}})
pipe.Stage(bson.D{{Key: "$out", Value: "results_collection"}})
```

## Executing

### Execute

Runs the pipeline and decodes all results:

```go
var results []bson.M
err := goodm.NewPipeline(&User{}).
    Match(bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}}).
    Group(bson.D{
        {Key: "_id", Value: "$role"},
        {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
    }).
    Sort(bson.D{{Key: "count", Value: -1}}).
    Execute(ctx, &results)
```

Results can be decoded into typed structs:

```go
type RoleCount struct {
    Role  string `bson:"_id"`
    Count int    `bson:"count"`
}

var counts []RoleCount
err := pipe.Execute(ctx, &counts)
```

### Cursor

Returns a raw `*mongo.Cursor` for streaming large result sets:

```go
cursor, err := pipe.Cursor(ctx)
if err != nil {
    log.Fatal(err)
}
defer cursor.Close(ctx)

for cursor.Next(ctx) {
    var doc bson.M
    cursor.Decode(&doc)
}
```

## Inspecting Stages

```go
stages := pipe.Stages() // []bson.D
```

## Full Example

Users per role with average age, sorted by count:

```go
type RoleStat struct {
    Role   string  `bson:"_id"`
    Count  int     `bson:"count"`
    AvgAge float64 `bson:"avg_age"`
}

var stats []RoleStat
err := goodm.NewPipeline(&User{}).
    Match(bson.D{{Key: "verified", Value: true}}).
    Group(bson.D{
        {Key: "_id", Value: "$role"},
        {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
        {Key: "avg_age", Value: bson.D{{Key: "$avg", Value: "$age"}}},
    }).
    Sort(bson.D{{Key: "count", Value: -1}}).
    Execute(ctx, &stats)
```
