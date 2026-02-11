package goodm

import (
	"context"
	"fmt"
	"sync"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	dbMu     sync.RWMutex
	globalDB *mongo.Database
)

// Connect establishes a connection to MongoDB and returns the database handle.
// It also stores the database reference globally for use by Enforce and the CLI.
func Connect(ctx context.Context, uri string, dbName string) (*mongo.Database, error) {
	clientOpts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("goodm: failed to connect: %w", err)
	}

	// Ping to verify connection
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("goodm: failed to ping: %w", err)
	}

	db := client.Database(dbName)

	dbMu.Lock()
	globalDB = db
	dbMu.Unlock()

	return db, nil
}

// DB returns the globally stored database reference.
// Returns nil if Connect has not been called.
func DB() *mongo.Database {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return globalDB
}
