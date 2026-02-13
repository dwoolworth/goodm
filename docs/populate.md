# Population

Population resolves `ref=` tagged fields by fetching the referenced documents from their collections. It works like Mongoose's `.populate()`.

## Setup

Tag an `ObjectID` field with `ref=collection` to mark it as a reference:

```go
type Post struct {
    goodm.Model `bson:",inline"`
    Title       string        `bson:"title"  goodm:"required"`
    AuthorID    bson.ObjectID `bson:"author" goodm:"ref=users"`
}

type User struct {
    goodm.Model `bson:",inline"`
    Name        string `bson:"name" goodm:"required"`
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

## Behavior

- **Zero refs** are skipped — if the `ObjectID` field is zero, the target struct is left untouched.
- **Dangling refs** (ID points to a nonexistent document) are skipped silently. The target struct remains at its zero value.
- **Missing field** or **no ref tag** returns an error immediately.

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
