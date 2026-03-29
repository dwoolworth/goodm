# Validation

goodm validates models automatically on `Create` and `Update`. Validation runs after hooks and before the database write.

## Validation Rules

### Required

Fields tagged `required` must be non-zero. Go's zero values are: `""` for strings, `0` for ints, `false` for bools, zero `bson.ObjectID`, zero `time.Time`.

```go
Name string `bson:"name" goodm:"required"`
```

### Enum

Fields tagged `enum=a|b|c` must contain one of the listed values (pipe-separated). Only validated when the field is non-zero.

```go
Status string `bson:"status" goodm:"enum=draft|published|archived"`
```

### Min / Max

Numeric fields tagged `min=N` or `max=N` are bounded. Only validated when non-zero.

```go
Age   int `bson:"age"   goodm:"min=13,max=120"`
Price int `bson:"price" goodm:"min=0"`
```

### Immutable

Fields tagged `immutable` cannot change after creation. On `Update`, goodm fetches the existing document and compares each immutable field using `reflect.DeepEqual`.

```go
Username string `bson:"username" goodm:"required,immutable"`
```

## ValidationErrors

When validation fails, a `ValidationErrors` (slice of `ValidationError`) is returned:

```go
err := goodm.Create(ctx, user)

var ve goodm.ValidationErrors
if errors.As(err, &ve) {
    for _, e := range ve {
        fmt.Printf("%s: %s\n", e.Field, e.Message)
        // e.Field is the bson field name (e.g. "email")
        // e.Message describes the violation
    }
}
```

Example messages:
- `"field is required"`
- `"value \"xyz\" is not in enum [draft published archived]"`
- `"value 5 is less than minimum 13"`
- `"value 200 exceeds maximum 120"`
- `"field is immutable and cannot be changed"`

## Subdocument Validation

Validation recurses into nested structs and slice elements. Error paths use dot notation for nested fields and bracket notation for slice indexes:

```go
order := &Order{
    Name: "Order1",
    Address: Address{Street: ""}, // required — will fail
    Items: []OrderItem{
        {Name: "", Quantity: 2}, // required — will fail
    },
}
err := goodm.Create(ctx, order)

// Errors:
// - Field: "address.street", Message: "field is required"
// - Field: "address.city",   Message: "field is required"
// - Field: "items[0].name",  Message: "field is required"
```

Path format examples:
- `address.street` — nested struct field
- `items[0].name` — first element of a slice
- `shipping.address.city` — deeply nested (subdoc within subdoc)

Nil pointer subdocuments are skipped during inner validation (the field-level `required` check catches their absence). Empty slices produce no inner validation errors.

## When Validation Runs

| Operation | Validates? | Immutable Check? |
|-----------|-----------|-----------------|
| `Create` | Yes | No (no prior state) |
| `CreateMany` | Yes (per model) | No |
| `Update` | Yes | Yes (fetches existing doc) |
| `UpdateOne` | No | No |
| `UpdateMany` | No | No |

## Manual Validation

You can validate a model without performing a database operation:

```go
schema, _ := goodm.Get("User")
errs := goodm.Validate(user, schema)
if len(errs) > 0 {
    // handle validation errors
}
```
