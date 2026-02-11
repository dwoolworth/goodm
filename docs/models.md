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

`goodm.Model` provides three fields automatically:

| Field | BSON | Type | Behavior |
|-------|------|------|----------|
| `ID` | `_id` | `bson.ObjectID` | Auto-generated on Create if zero |
| `CreatedAt` | `created_at` | `time.Time` | Set on Create (only if zero) |
| `UpdatedAt` | `updated_at` | `time.Time` | Set on Create, refreshed on Update |

Always embed with `bson:",inline"` to flatten the fields into the document.

## Tag Reference

Tags are specified in the `goodm` struct tag, comma-separated:

### `unique`

Creates a unique index on this field. Enforced at the database level.

```go
Email string `bson:"email" goodm:"unique"`
```

### `index`

Creates a non-unique index on this field.

```go
Category string `bson:"category" goodm:"index"`
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

Annotates the default value. This is metadata for documentation and code generation — goodm does not automatically apply defaults.

```go
Role string `bson:"role" goodm:"default=user"`
```

### `enum=a|b|c`

Restricts the field to one of the listed values (pipe-separated). Validated on Create and Update.

```go
Status string `bson:"status" goodm:"enum=draft|published|archived"`
```

### `min=N` / `max=N`

Numeric boundaries for int/float fields. Validated on Create and Update.

```go
Age   int `bson:"age"   goodm:"min=13,max=120"`
Price int `bson:"price" goodm:"min=0"`
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

## Compound Indexes

For multi-field indexes, implement the `Indexable` interface:

```go
func (u *User) Indexes() []goodm.CompoundIndex {
    return []goodm.CompoundIndex{
        goodm.NewCompoundIndex("email", "role"),           // non-unique
        goodm.NewUniqueCompoundIndex("tenant", "username"), // unique
    }
}
```

Compound indexes are created by `Enforce()` alongside single-field indexes.

## Registration

Models must be registered before use. The convention is to register in `init()`:

```go
func init() {
    if err := goodm.Register(&User{}, "users"); err != nil {
        panic(err)
    }
}
```

`Register` parses the struct tags, detects hook implementations, and stores the schema in an internal registry. The registry is used by all CRUD operations to:

- Look up the collection name for a model type
- Validate fields on write operations
- Enforce immutable fields on updates
- Detect compound indexes

## Inspecting Schemas

Retrieve registered schemas programmatically:

```go
// Get a specific schema
schema, ok := goodm.Get("User")

// Get all registered schemas
all := goodm.GetAll()
for name, schema := range all {
    fmt.Printf("%s -> %s\n", name, schema.Collection)
}
```

Or use the CLI:

```bash
goodm inspect
```
