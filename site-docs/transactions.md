# Transactions

goodm wraps MongoDB multi-document transactions with a simple callback API. All goodm CRUD operations called within the callback automatically participate in the transaction.

## Requirements

Transactions require a **MongoDB replica set** (or sharded cluster). They are not available on standalone instances.

## Usage

```go
err := goodm.WithTransaction(ctx, func(ctx context.Context) error {
    // All operations here are part of the same transaction.
    // Use the ctx passed to the callback — it carries the session.

    order := &Order{UserID: user.ID, Total: 9999}
    if err := goodm.Create(ctx, order); err != nil {
        return err // transaction will be aborted
    }

    user.Balance -= 9999
    if err := goodm.Update(ctx, user); err != nil {
        return err // transaction will be aborted
    }

    return nil // transaction will be committed
})
```

## Behavior

- If the callback returns `nil`, the transaction is **committed**.
- If the callback returns an error, the transaction is **aborted** and all writes are rolled back.
- Transient transaction errors are **retried automatically** by the MongoDB driver.

## How It Works

`WithTransaction` does the following:

1. Gets the `*mongo.Client` from the global database (or from `TransactionOptions.DB`)
2. Starts a new session via `client.StartSession()`
3. Calls `session.WithTransaction()` with your callback
4. The MongoDB driver passes a session-aware context to the callback
5. All goodm operations using that context participate in the transaction

The key is using the `ctx` parameter from the callback, not the outer context:

```go
goodm.WithTransaction(outerCtx, func(ctx context.Context) error {
    // Use THIS ctx, not outerCtx
    return goodm.Create(ctx, doc)
})
```

## Options

Override the database connection:

```go
goodm.WithTransaction(ctx, fn, goodm.TransactionOptions{
    DB: otherDB,
})
```

## Error Handling

```go
err := goodm.WithTransaction(ctx, func(ctx context.Context) error {
    // ...
    return nil
})

if errors.Is(err, goodm.ErrNoDatabase) {
    // Connect() hasn't been called
}
if err != nil {
    // Transaction failed (could be write conflict, network error, etc.)
}
```

## Example: Transfer Between Accounts

```go
func Transfer(ctx context.Context, fromID, toID bson.ObjectID, amount int) error {
    return goodm.WithTransaction(ctx, func(ctx context.Context) error {
        from := &Account{}
        if err := goodm.FindOne(ctx, bson.D{{Key: "_id", Value: fromID}}, from); err != nil {
            return err
        }
        if from.Balance < amount {
            return fmt.Errorf("insufficient funds")
        }

        to := &Account{}
        if err := goodm.FindOne(ctx, bson.D{{Key: "_id", Value: toID}}, to); err != nil {
            return err
        }

        from.Balance -= amount
        to.Balance += amount

        if err := goodm.Update(ctx, from); err != nil {
            return err
        }
        return goodm.Update(ctx, to)
    })
}
```
