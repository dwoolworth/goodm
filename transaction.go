package goodm

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TransactionOptions configures the WithTransaction operation.
type TransactionOptions struct {
	DB *mongo.Database

	// Transaction passes driver-level transaction options (read concern,
	// write concern, read preference, max commit time).
	Transaction *options.TransactionOptionsBuilder
}

// WithTransaction executes fn within a MongoDB transaction. All goodm CRUD
// operations called within fn automatically participate in the transaction
// via the session-aware context.
//
// If fn returns an error, the transaction is aborted. If fn succeeds, the
// transaction is committed. Transient transaction errors are retried
// automatically by the driver.
//
// If ctx already carries a session (a nested WithTransaction call), fn runs
// inside the existing transaction instead of starting a second one, so an
// error from the inner fn aborts the whole outer transaction.
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
	if mongo.SessionFromContext(ctx) != nil {
		return fn(ctx)
	}

	var opt TransactionOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	db, err := getDB(opt.DB)
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

	var txnOpts []options.Lister[options.TransactionOptions]
	if opt.Transaction != nil {
		txnOpts = append(txnOpts, opt.Transaction)
	}
	_, err = session.WithTransaction(ctx, func(ctx context.Context) (interface{}, error) {
		return nil, fn(ctx)
	}, txnOpts...)
	if err != nil {
		return fmt.Errorf("goodm: transaction failed: %w", err)
	}

	return nil
}
