# Validation

goodm validates models automatically on `Create` and `Update`. Validation runs after hooks and before the database write.

## Validation Rules

### Required

Fields tagged `required` must be non-zero. Go's zero values are: `""` for strings, `0` for ints, `false` for bools, zero `bson.ObjectID`, zero `time.Time`. A value whose own `IsZero` method reports zero also counts as missing (a non-nil `*time.Time` pointing at the zero time), as the driver's `omitempty` treats it.

```go
Name string `bson:"name" goodm:"required"`
```

### Enum

Fields tagged `enum=a|b|c` must contain one of the listed values (pipe-separated). Only validated when the field is non-zero. On a slice or array field every element must be one of the values, including empty strings inside a non-empty slice; a nil pointer element is skipped like an unset scalar, and an offending element is reported as `tags[2]`.

```go
Status string `bson:"status" goodm:"enum=draft|published|archived"`
```

### Min / Max

Fields tagged `min=N` or `max=N` are bounded: numbers (ints and floats) by value, strings by length in characters (not bytes), slices and maps by length. Bounds may be fractional. The zero value is checked like any other, so `min=1` rejects `0`, `min=0` rejects `-0.5`, and a nil or empty slice has length 0; only a nil pointer counts as absent.

```go
Age    int     `bson:"age"    goodm:"min=13,max=120"`
Price  float64 `bson:"price"  goodm:"min=0"`
Rating *int    `bson:"rating" goodm:"min=1,max=5"` // optional: nil is skipped
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
| `SaveMany` | Yes (per model) | Yes (one fetch for the batch) |
| `UpdateFields` | Yes (the values being set) | Yes (immutable fields are rejected) |
| `FindOneAndUpdate` | No | No |
| `UpdateOne` | No | No |
| `UpdateMany` | No | No |
| `Bulk` | No | No |

## Server-Side Validation

Every rule above runs in your process, so a write from another application, a shell session, or a raw driver call bypasses it. `Schema.JSONSchema()` turns the same struct into a MongoDB `$jsonSchema` validator, and `Enforce` can install it on the collection:

```go
goodm.Enforce(ctx, db, goodm.EnforceOptions{
    Validator: goodm.ValidatorApply, // set each collection's validator from its model
})
```

| Policy | Effect |
|--------|--------|
| `ValidatorIgnore` (default) | Collection validators are left alone |
| `ValidatorApply` | Each collection's validator is set to the model's schema (creating the collection if needed); a collection whose validator already matches is untouched, and one with a different validator is overwritten |
| `ValidatorVerify` | An `EnforcementError` if a collection's validator, validation level, or validation action differs from the model; nothing is changed |

`ValidationLevel` (`strict` by default, or `moderate` to skip existing documents that already fail) and `ValidationAction` (`error` by default, or `warn` to log and accept) are set alongside it. `Enforce` refuses to derive a validator for a collection registered for more than one model.

How the struct maps to the schema:

| Struct | `$jsonSchema` |
|--------|---------------|
| Go type | `bsonType` as the driver encodes it: integers `["int", "long"]` (32-bit kinds just `int`), floats `double`, `time.Time` `date`, `bson.ObjectID` `objectId`, `[]byte` `binData`, `url.URL` `string`, the `bson` primitive types their own types, structs `object` with their own `properties`, slices `array` with `items`, maps `object` with `additionalProperties`; unsigned integers get `minimum: 0` |
| Pointers, slices, maps, `interface{}`, `bson.D`/`M`/`Raw` | Also allow `null`, because `UpdateFields` stores a nil as null |
| Types with their own `MarshalBSON` or `MarshalBSONValue` | No constraint, since the schema cannot know what they write |
| `[]byte` and `[N]byte` | `binData`; a slice or array of a named byte type is an `array`, as the driver stores it |
| `required` | Listed in the object's `required` |
| `enum=a\|b` | `enum` (on the `items` for a slice); numbers and bools are parsed. The field's zero value is allowed too unless it is `required`, because `Validate` treats a zero value as unset and a field without `omitempty` is written as its zero value; a pointer allows `null` |
| `min`/`max` | `minimum`/`maximum` for numbers, `minLength`/`maxLength` for strings (counted in characters), `minItems`/`maxItems` for arrays, `minProperties`/`maxProperties` for maps |
| `bson:"-"` fields | Omitted |

Fields the struct does not declare are allowed, as drift detection treats them as warnings. The server only checks that a `required` field is present, while `Validate` also rejects its zero value, so goodm's own checks remain the stricter of the two: a document goodm writes always passes the validator it generated, with two exceptions. A `FindOneAndUpdate` upsert whose filter has no `_id` makes the server generate an ObjectID, which the validator of a model with another `_id` type rejects. And codecs registered on the client or database, and client `BSONOptions` that change how fields are named or encoded, are not visible to the schema. A model stored through such codecs should set `SkipValidator: true` in its `CollectionOptions`, which makes `Enforce` leave that collection's validator alone under every policy:

```go
func (p *Price) CollectionOptions() goodm.CollectionOptions {
    return goodm.CollectionOptions{SkipValidator: true}
}
```

A write the server rejects returns a `*DocumentValidationError`, matched by `errors.Is(err, goodm.ErrDocumentValidation)`, from every write path including the raw operations, `FindOneAndUpdate`, and the bulk operations (per item in the `*BulkError`). Its `Details` field holds the server's `errInfo` document, which names the failing rule and the value it saw:

```go
if errors.Is(err, goodm.ErrDocumentValidation) {
    var dve *goodm.DocumentValidationError
    errors.As(err, &dve)
    log.Printf("rejected by %s: %s", dve.Collection, dve.Details)
}
```

`goodm inspect --json-schema` prints each model's validator as JSON, for use with `collMod` or `createCollection` by hand.

## Manual Validation

You can validate a model without performing a database operation:

```go
schema, _ := goodm.SchemaOf(&User{})
errs := goodm.Validate(user, schema)
if len(errs) > 0 {
    // handle validation errors
}
```
