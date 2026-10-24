package goodm

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/session"
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
// If ctx already carries a session with a transaction in progress (a nested
// WithTransaction call), fn runs inside that transaction instead of starting
// a second one, so an error from the inner fn aborts the whole outer
// transaction; the inner call's options are ignored. If ctx carries a session
// without an active transaction, a transaction is started on that session.
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
	var opt TransactionOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	var txnOpts []options.Lister[options.TransactionOptions]
	if opt.Transaction != nil {
		txnOpts = append(txnOpts, opt.Transaction)
	}
	callback := func(ctx context.Context) (interface{}, error) {
		return nil, fn(ctx)
	}

	if sess := mongo.SessionFromContext(ctx); sess != nil {
		_, err := sess.WithTransaction(ctx, callback, txnOpts...)
		if errors.Is(err, session.ErrTransactInProgress) {
			// Nested call: join the transaction already running on this session.
			return fn(ctx)
		}
		if err != nil {
			return fmt.Errorf("goodm: transaction failed: %w", err)
		}
		return nil
	}

	db, err := getDB(opt.DB)
	if err != nil {
		return err
	}

	client := db.Client()
	if client == nil {
		return ErrNoDatabase
	}

	sess, err := client.StartSession()
	if err != nil {
		return fmt.Errorf("goodm: failed to start session: %w", err)
	}
	defer sess.EndSession(ctx)

	if _, err := sess.WithTransaction(ctx, callback, txnOpts...); err != nil {
		return fmt.Errorf("goodm: transaction failed: %w", err)
	}
	return nil
}
