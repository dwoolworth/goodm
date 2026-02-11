# Hooks

Hooks are lifecycle callbacks that run before or after CRUD operations. Implement them as methods on your model struct.

## Available Hooks

| Hook | Triggered By | When |
|------|-------------|------|
| `BeforeCreate` | `Create`, `CreateMany` | Before insert, after timestamps and ID generation |
| `AfterCreate` | `Create`, `CreateMany` | After successful insert |
| `BeforeSave` | `Update` | Before replace, after fetching existing doc |
| `AfterSave` | `Update` | After successful replace |
| `BeforeDelete` | `Delete` | Before delete |
| `AfterDelete` | `Delete` | After successful delete |

## Interfaces

Each hook is a single-method interface:

```go
type BeforeCreate interface {
    BeforeCreate(ctx context.Context) error
}

type AfterCreate interface {
    AfterCreate(ctx context.Context) error
}

type BeforeSave interface {
    BeforeSave(ctx context.Context) error
}

type AfterSave interface {
    AfterSave(ctx context.Context) error
}

type BeforeDelete interface {
    BeforeDelete(ctx context.Context) error
}

type AfterDelete interface {
    AfterDelete(ctx context.Context) error
}
```

## Implementing Hooks

Add the hook method to your model with a pointer receiver:

```go
type User struct {
    goodm.Model `bson:",inline"`
    Email       string `bson:"email" goodm:"unique,required"`
    Name        string `bson:"name"  goodm:"required"`
}

func (u *User) BeforeCreate(ctx context.Context) error {
    // Normalize email
    u.Email = strings.ToLower(u.Email)
    return nil
}

func (u *User) AfterCreate(ctx context.Context) error {
    log.Printf("User created: %s", u.Email)
    return nil
}

func (u *User) BeforeSave(ctx context.Context) error {
    log.Printf("Saving user: %s", u.ID.Hex())
    return nil
}

func (u *User) BeforeDelete(ctx context.Context) error {
    log.Printf("Deleting user: %s", u.ID.Hex())
    return nil
}
```

## Error Handling

If a hook returns an error, the operation is aborted and the error is returned to the caller:

```go
func (u *User) BeforeCreate(ctx context.Context) error {
    if strings.Contains(u.Email, "+") {
        return fmt.Errorf("email aliases not allowed")
    }
    return nil
}
```

## Execution Order

For `Create`:
```
ID generation → Timestamps → BeforeCreate → Validate → InsertOne → AfterCreate
```

For `Update`:
```
Fetch existing → BeforeSave → Immutable check → Validate → UpdatedAt → ReplaceOne → AfterSave
```

For `Delete`:
```
BeforeDelete → DeleteOne → AfterDelete
```

## Which Operations Run Hooks?

| Operation | Hooks | Notes |
|-----------|-------|-------|
| `Create` | BeforeCreate, AfterCreate | Full lifecycle |
| `CreateMany` | BeforeCreate, AfterCreate | Per model in the batch |
| `Update` | BeforeSave, AfterSave | Full lifecycle |
| `Delete` | BeforeDelete, AfterDelete | Full lifecycle |
| `UpdateOne` | None | Raw passthrough |
| `DeleteOne` | None | Raw passthrough |
| `UpdateMany` | None | Raw passthrough |
| `DeleteMany` | None | Raw passthrough |
| `FindOne` | None | Read-only |
| `Find` | None | Read-only |
