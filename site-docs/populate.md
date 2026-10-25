# Population

Population resolves `ref=` tagged fields by fetching the referenced documents from their collections. It works like Mongoose's `.populate()`.

## Setup

Tag an `ObjectID` field with `ref=collection` to mark it as a reference. Both single refs and array refs are supported:

```go
type Post struct {
    goodm.Model `bson:",inline"`
    Title       string            `bson:"title"  goodm:"required"`
    AuthorID    bson.ObjectID     `bson:"author" goodm:"ref=users"`
    TagIDs      []bson.ObjectID   `bson:"tags"   goodm:"ref=tags"`
}

type User struct {
    goodm.Model `bson:",inline"`
    Name        string `bson:"name" goodm:"required"`
}

type Tag struct {
    goodm.Model `bson:",inline"`
    Label       string `bson:"label" goodm:"required"`
}
```

## Using Populate

After loading a document, call `Populate` with a `Refs` map. Keys are bson field names, values are pointers to structs where the referenced documents will be decoded:

```go
// Load the post
post := &Post{}
goodm.FindOne(ctx, bson.D{{Key: "title", Value: "Hello World"}}, post)

// Populate the author
author := &User{}
err := goodm.Populate(ctx, post, goodm.Refs{
    "author": author,
})

fmt.Println(author.Name) // "Alice"
```

## Multiple References

Populate multiple refs in a single call:

```go
type Post struct {
    goodm.Model  `bson:",inline"`
    Title        string        `bson:"title"    goodm:"required"`
    AuthorID     bson.ObjectID `bson:"author"   goodm:"ref=users"`
    CategoryID   bson.ObjectID `bson:"category" goodm:"ref=categories"`
}

author := &User{}
category := &Category{}
err := goodm.Populate(ctx, post, goodm.Refs{
    "author":   author,
    "category": category,
})
```

## Array References

For `[]bson.ObjectID` fields, pass a pointer to a slice as the target. All referenced documents are fetched in a single `$in` query:

```go
var tags []Tag
err := goodm.Populate(ctx, post, goodm.Refs{
    "tags": &tags,
})

for _, tag := range tags {
    fmt.Println(tag.Label)
}
```

You can mix single and array refs in one call:

```go
author := &User{}
var tags []Tag
err := goodm.Populate(ctx, post, goodm.Refs{
    "author": author,
    "tags":   &tags,
})
```

## Nested References

A ref inside a subdocument is named by its dotted path of bson names. The path may pass through struct fields, pointer fields (a nil pointer yields nothing), and slices of subdocuments:

```go
type LineItem struct {
    ProductID bson.ObjectID `bson:"product" goodm:"ref=products"`
    Qty       int           `bson:"qty"`
}

type Shipping struct {
    CarrierID bson.ObjectID `bson:"carrier" goodm:"ref=carriers"`
}

type Order struct {
    goodm.Model `bson:",inline"`
    Items       []LineItem `bson:"items"`
    Ship        Shipping   `bson:"ship"`
}

carrier := &Carrier{}
var products []Product
err := goodm.Populate(ctx, order, goodm.Refs{
    "ship.carrier":  carrier,   // one ref: a struct pointer
    "items.product": &products, // every item's ref: a slice pointer
})
```

When the path crosses a slice without naming an element, the refs of every element are collected (distinct, zero ids and nil elements skipped) and fetched in one `$in` query, so the target must be a pointer to a slice; a struct pointer is an error. A numeric segment selects one element instead, and then a struct pointer is fine:

```go
first := &Product{}
err := goodm.Populate(ctx, order, goodm.Refs{"items.0.product": first})
```

An index past the end of the slice populates nothing. The same paths work with `BatchPopulate`:

```go
var products []Product
err := goodm.BatchPopulate(ctx, orders, "items.product", &products)
```

## Behavior

- **Zero refs** are skipped — if the `ObjectID` field is zero, the target struct is left untouched.
- **Empty/nil array refs** are skipped — the target slice is left as it was.
- **Dangling refs** (ID points to a nonexistent document) are skipped silently. The target struct remains at its zero value. Documents fetched for an array ref come back in the server's order, not the order of the ids.
- **Targets** must be non-nil pointers, and a path that can yield several ids (an array ref, or a path through a slice without an index) needs a pointer to a slice. This is checked from the path before any lookup, so a wrong target type fails even when the field is empty.
- **Missing field**, **no ref tag**, or a **path through a field that is not a subdocument** returns an error immediately.

## Options

Override the database connection:

```go
goodm.Populate(ctx, post, goodm.Refs{"author": author}, goodm.PopulateOptions{
    DB: otherDB,
})
```

## Batch Population

Use `BatchPopulate` to resolve a ref field across a slice of models in a single `$in` query, avoiding N+1 overhead:

```go
var posts []Post
goodm.Find(ctx, bson.D{}, &posts)

var authors []User
err := goodm.BatchPopulate(ctx, posts, "author", &authors)
```

This collects unique IDs from the `author` field, fetches all referenced documents in one query, and decodes them into the `authors` slice. To map authors back to posts, build a lookup map:

```go
authorMap := make(map[bson.ObjectID]*User)
for i := range authors {
    authorMap[authors[i].ID] = &authors[i]
}

for _, post := range posts {
    author := authorMap[post.AuthorID]
    // ...
}
```

> **Note:** `Populate` makes one query per ref field on a single model. For slices of models, always prefer `BatchPopulate` to avoid N+1 queries.
