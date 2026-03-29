# Aggregation Pipeline

goodm provides a fluent builder for MongoDB aggregation pipelines. Chain stages together and execute against a collection.

## Creating a Pipeline

```go
pipe := goodm.NewPipeline(&User{})
```

The model parameter determines the collection. Pass options to override the database:

```go
pipe := goodm.NewPipeline(&User{}, goodm.PipelineOptions{DB: otherDB})
```

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
```

### Lookup

Left outer join:

```go
pipe.Lookup("orders", "user_id", "_id", "user_orders")
// Joins the "orders" collection where orders.user_id == users._id
// Results stored in "user_orders" array
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
