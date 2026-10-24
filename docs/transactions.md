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

## Nested Calls

If the context already carries a session with a transaction in progress, a nested `WithTransaction` runs its callback inside that transaction instead of starting a second one, and ignores its own options. An error from the inner callback therefore aborts the whole outer transaction, and the inner writes are never committed on their own. This lets helpers that wrap themselves in `WithTransaction` be composed freely. A context that carries a session without an active transaction gets a transaction started on that session.

## Batch Operations

A `SaveMany` or `CreateMany` error inside the callback must be returned, not ignored. When the server rejected a write, the transaction is already aborted, and returning nil makes the driver retry the callback until its two-minute limit instead of failing fast. See [Bulk Operations](bulk.md) for how each batch operation reports its items inside a transaction.

## Options

Override the database connection or pass driver transaction options (read concern, write concern, read preference, max commit time):

```go
goodm.WithTransaction(ctx, fn, goodm.TransactionOptions{
    DB: otherDB,
    Transaction: options.Transaction().
        SetReadConcern(readconcern.Snapshot()).
        SetWriteConcern(writeconcern.Majority()),
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
