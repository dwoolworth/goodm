# Models & Tags

## Model Definition

A goodm model is a Go struct that embeds `goodm.Model` and is registered with a collection name. The struct is the single source of truth for your database schema.

```go
type User struct {
    goodm.Model `bson:",inline"`
    Email       string        `bson:"email"    goodm:"unique,required"`
    Name        string        `bson:"name"     goodm:"required,immutable"`
    Role        string        `bson:"role"     goodm:"enum=admin|user|mod,default=user"`
    Age         int           `bson:"age"      goodm:"min=13,max=120"`
    Profile     bson.ObjectID `bson:"profile"  goodm:"ref=profiles"`
    Verified    bool          `bson:"verified" goodm:"default=false"`
}

func init() {
    goodm.Register(&User{}, "users")
}
```

## Base Model

`goodm.Model` provides four fields automatically. Embedding it is the common case, but any struct with an `_id` field can be a model; see [Custom ID Types](#custom-id-types).

| Field | BSON | Type | Behavior |
|-------|------|------|----------|
| `ID` | `_id` | `bson.ObjectID` | Auto-generated on Create if zero |
| `CreatedAt` | `created_at` | `time.Time` | Set on Create (only if zero) |
| `UpdatedAt` | `updated_at` | `time.Time` | Set on Create, refreshed on Update |
| `Version` | `__v` | `int` | Set to 0 on Create, incremented on each Update (optimistic concurrency) |

Always embed with `bson:",inline"` to flatten the fields into the document. goodm follows the driver's default registry here: any exported struct or pointer-to-struct field tagged `inline`, embedded or named, is flattened into the document; an exported embedded struct without the tag is a subdocument field named by its tag or its lowercased type name; unexported fields and embeds are ignored entirely; an inline map declares no schema fields. `Register` rejects a model that stores two fields under the same bson name. The managed fields (`_id`, `created_at`, `updated_at`, `__v`) are located by those bson names, not by Go field names.

## Tag Reference

Tags are specified in the `goodm` struct tag, comma-separated:

### `unique`

Creates a unique index on this field. Enforced at the database level. A write that violates it returns a `*DuplicateKeyError` (`errors.Is(err, goodm.ErrDuplicateKey)`).

```go
Email string `bson:"email" goodm:"unique"`
```

### `index` / `index=kind`

Creates a non-unique index on this field. A bare `index` is ascending; give a kind for other index types:

```go
Category string    `bson:"category" goodm:"index"`          // ascending
Score    int       `bson:"score"    goodm:"index=-1"`       // descending
Tenant   string    `bson:"tenant"   goodm:"index=hashed"`   // hashed
Body     string    `bson:"body"     goodm:"index=text"`     // text
Location bson.D    `bson:"loc"      goodm:"index=2dsphere"` // geospatial
```

Tags on fields inside subdocuments create dotted-path indexes (`address.city_1`), including for slices of structs (multikey). Take care with `unique` inside an array: every element is indexed, and documents whose array is empty or missing all index as `null` and collide with each other. Use `unique,sparse` or a partial filter from `Indexes()` for those.

### `sparse`

Makes the field's index sparse: documents missing the field are left out, so a `unique,sparse` index allows any number of documents without the field.

```go
Slug string `bson:"slug" goodm:"unique,sparse"`
```

### `ttl=duration`

Creates a TTL index that expires documents once the field's date is older than the duration. Accepts a Go duration (`24h`, `30m`) or a number of seconds. `ttl=0` expires documents at the time stored in the field.

```go
ExpiresAt time.Time `bson:"expires_at" goodm:"ttl=24h"`
```

### `required`

Field must be non-zero on Create and Update. Zero means Go's zero value: `""` for strings, `0` for ints, `false` for bools, zero `ObjectID`, etc.

```go
Name string `bson:"name" goodm:"required"`
```

### `immutable`

Field cannot be changed after the document is created. On Update, goodm fetches the existing document and compares immutable fields using `reflect.DeepEqual`. If any differ, a `ValidationError` is returned.

```go
Username string `bson:"username" goodm:"immutable"`
```

### `default=X`

Sets the default value for a field. During `Create` and `CreateMany`, if the field is zero-valued, goodm sets it to this default before hooks and validation run. Supported types: string, bool, int/int8-64, uint/uint8-64, float32/float64.

```go
Role string `bson:"role" goodm:"default=user"`
```

### `enum=a|b|c`

Restricts the field to one of the listed values (pipe-separated). Validated on Create and Update.

```go
Status string `bson:"status" goodm:"enum=draft|published|archived"`
```

### `min=N` / `max=N`

Bounds on numeric values (ints and floats, compared as numbers) or on the length of strings, slices, and maps. Validated on Create, Update, SaveMany, and the values passed to UpdateFields. Bounds may be fractional (`min=0.5`).

```go
Age   int     `bson:"age"   goodm:"min=13,max=120"`
Price float64 `bson:"price" goodm:"min=0"`
Bio   string  `bson:"bio"   goodm:"max=500"`
```

Bounds apply to the zero value too: `min=1` rejects `0`. For an optional bounded field use a pointer; a nil pointer is treated as absent and is not checked.

```go
Rating *int `bson:"rating" goodm:"min=1,max=5"` // nil is fine, 0 is not
```

### `ref=collection`

Marks a `bson.ObjectID` field as a reference to a document in another collection. Used by `Populate()` to resolve references.

```go
AuthorID bson.ObjectID `bson:"author" goodm:"ref=users"`
```

## Combining Tags

Tags are comma-separated and can be combined freely:

```go
Email string `bson:"email" goodm:"unique,required,index"`
SKU   string `bson:"sku"   goodm:"unique,required,immutable"`
```

## Indexes()

Field tags cover single-field indexes. For compound indexes, custom names, partial filters, and collations, implement the `Indexable` interface:

```go
func (u *User) Indexes() []goodm.CompoundIndex {
    return []goodm.CompoundIndex{
        goodm.NewCompoundIndex("tenant", "-created_at"),     // tenant asc, created_at desc
        goodm.NewUniqueCompoundIndex("tenant", "username"),  // unique
        {
            Fields:        []string{"email"},
            Unique:        true,
            Name:          "active_email",
            PartialFilter: bson.D{{Key: "deleted_at", Value: nil}},
        },
        {
            Fields:    []string{"name"},
            Collation: &options.Collation{Locale: "en", Strength: 2}, // case-insensitive
        },
        {
            Fields:             []string{"last_seen"},
            ExpireAfterSeconds: goodm.TTL(30 * 24 * time.Hour),
        },
    }
}
```

Each entry in `Fields` is a bson path with an optional marker:

| Entry | Key |
|-------|-----|
| `"email"` | `{email: 1}` |
| `"-created_at"` | `{created_at: -1}` |
| `"title:text"` | `{title: "text"}` |
| `"tenant:hashed"` | `{tenant: "hashed"}` |
| `"loc:2dsphere"` | `{loc: "2dsphere"}` |

`CompoundIndex` options:

| Field | Effect |
|-------|--------|
| `Unique` | Unique index |
| `Sparse` | Skip documents missing every key |
| `Name` | Override MongoDB's default name (`email_1_role_-1`) |
| `ExpireAfterSeconds` | TTL in seconds; build with `goodm.TTL(d)` |
| `PartialFilter` | `bson.D`/`bson.M` filter restricting the index |
| `Collation` | `*options.Collation` for string comparison |

Despite the name a `CompoundIndex` may list a single field, which is how you attach these options to one field.

### Index definitions

Tags and `Indexes()` resolve to one list of `IndexDefinition` values via `schema.IndexDefinitions()`. `Enforce`, `Migrate`, and the CLI all work from that list, so an index means the same thing everywhere. An existing index with the expected name but different keys, uniqueness, sparseness, TTL, partial filter, or collation is a mismatch; see `EnforceOptions.IndexMismatchPolicy`.

## Collection Options (Read/Write Concern)

For per-schema read preference, read concern, and write concern, implement the `Configurable` interface:

```go
import (
    "go.mongodb.org/mongo-driver/v2/mongo/readpref"
    "go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func (u *User) CollectionOptions() goodm.CollectionOptions {
    return goodm.CollectionOptions{
        ReadPreference: readpref.SecondaryPreferred(),
        WriteConcern:   writeconcern.Majority(),
    }
}
```

All CRUD, bulk, and pipeline operations automatically use the configured options. This is useful for separating read/write patterns in clustered deployments — for example, routing reads for an analytics model to secondaries while keeping user writes on majority concern.

Available options:

| Field | Type | Example |
|-------|------|---------|
| `ReadPreference` | `*readpref.ReadPref` | `readpref.SecondaryPreferred()`, `readpref.Nearest()` |
| `ReadConcern` | `*readconcern.ReadConcern` | `readconcern.Majority()`, `readconcern.Local()` |
| `WriteConcern` | `*writeconcern.WriteConcern` | `writeconcern.Majority()`, `writeconcern.W1()` |

Models without `CollectionOptions()` use whatever concern is configured on the `*mongo.Database`.

## Subdocuments

Nested structs are treated as subdocuments. goodm recursively parses `goodm` tags on nested struct fields, so validation, defaults, and schema introspection work at any depth.

```go
type Address struct {
    Street string `bson:"street" goodm:"required"`
    City   string `bson:"city"   goodm:"required"`
    Zip    string `bson:"zip"    goodm:"default=00000"`
}

type OrderItem struct {
    Name     string `bson:"name"     goodm:"required"`
    Quantity int    `bson:"quantity" goodm:"min=1"`
}

type Order struct {
    goodm.Model `bson:",inline"`
    Name        string      `bson:"name"    goodm:"required"`
    Address     Address     `bson:"address" goodm:"required"`
    Items       []OrderItem `bson:"items"`
}
```

Subdocuments support:
- **Validation** — all `goodm` tags (`required`, `enum`, `min`, `max`) are enforced recursively. Errors use dotted paths: `"address.street"`, `"items[0].name"`.
- **Defaults** — `default=X` tags inside subdocuments are applied during `Create` and `CreateMany`.
- **Schema introspection** — `FieldSchema.SubFields` contains the parsed inner fields. `FieldSchema.IsSlice` is true for `[]struct` fields.
- **Immutability** — marking a subdocument field as `immutable` makes the entire subdocument immutable (compared with `reflect.DeepEqual`).

Subdocuments are NOT separate models — they don't have collections, hooks, or CRUD operations. They exist as part of their parent's schema.

Leaf types (`time.Time`, `bson.ObjectID`, `bson.Decimal128`) are never recursed into, even though they are structs.

## Registration

Models must be registered before use. The convention is to register in `init()`:

```go
func init() {
    if err := goodm.Register(&User{}, "users"); err != nil {
        panic(err)
    }
}
```

`Register` parses the struct tags, detects hook and interface implementations, and stores the schema in an internal registry keyed by the struct's type. The registry is used by all CRUD operations to:

- Look up the collection name for a model type
- Validate fields on write operations
- Enforce immutable fields on updates
- Detect compound indexes (`Indexable`)
- Apply per-schema collection options (`Configurable`)

Because schemas are keyed by type, two packages may each register a struct named `User`. Registering the same type twice is an error. `goodm.Unregister(&User{})` removes a schema again, which is handy in tests.

## Custom ID Types

The `_id` field is the struct field tagged `bson:"_id"`, found directly or through embedded structs tagged `bson:",inline"`; `Register` rejects a model that has none, and in particular one that embeds `goodm.Model` without the inline tag, because the driver would store its `_id` as a nested document. A field merely named `ID` is not enough, because the driver would store it as `id`. When the `_id` field is a `bson.ObjectID`, `Create` generates a value if it is zero. Any other type works, but you must set it yourself before `Create` or in a `BeforeCreate` hook; a zero `_id` of a non-ObjectID type is rejected rather than inserted as an empty key.

```go
type Setting struct {
    ID        string    `bson:"_id"`
    Value     string    `bson:"value" goodm:"required"`
    UpdatedAt time.Time `bson:"updated_at"`
}
```

Timestamps and versioning are applied when the struct has `CreatedAt`, `UpdatedAt`, and `Version` fields (as `goodm.Model` provides) and skipped otherwise.

## Inspecting Schemas

Retrieve registered schemas programmatically:

```go
// By type (never ambiguous)
schema, ok := goodm.SchemaOf(&User{})

// By struct name; ok is false if the name is unregistered or ambiguous
schema, ok := goodm.Get("User")

// All registered schemas, sorted by collection name
for _, schema := range goodm.GetAll() {
    fmt.Printf("%s -> %s\n", schema.QualifiedName(), schema.Collection)
    for _, idx := range schema.IndexDefinitions() {
        fmt.Printf("  index %s %v\n", idx.Name, idx.Keys)
    }
}
```

Or use the CLI:

```bash
goodm inspect
```
