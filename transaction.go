package goodm

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TransactionOptions configures the WithTransaction operation.
type TransactionOptions struct {
	DB *mongo.Database
}

// WithTransaction executes fn within a MongoDB transaction. All goodm CRUD
// operations called within fn automatically participate in the transaction
// via the session-aware context.
//
// If fn returns an error, the transaction is aborted. If fn succeeds, the
// transaction is committed. Transient transaction errors are retried
// automatically by the driver.
//
// Example:
//
//	err := goodm.WithTransaction(ctx, func(ctx context.Context) error {
//	    if err := goodm.Create(ctx, user); err != nil {
//	        return err
//	    }
//	    if err := goodm.Create(ctx, profile); err != nil {
//	        return err
//	    }
//	    return nil
//	})
func WithTransaction(ctx context.Context, fn func(ctx context.Context) error, opts ...TransactionOptions) error {
	var optDB *mongo.Database
	if len(opts) > 0 {
		optDB = opts[0].DB
	}
	db, err := getDB(optDB)
	if err != nil {
		return err
	}

	client := db.Client()
	if client == nil {
		return ErrNoDatabase
	}

	session, err := client.StartSession()
	if err != nil {
		return fmt.Errorf("goodm: failed to start session: %w", err)
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(ctx context.Context) (interface{}, error) {
		return nil, fn(ctx)
	})
	if err != nil {
		return fmt.Errorf("goodm: transaction failed: %w", err)
	}

	return nil
}
