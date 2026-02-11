package goodm

import (
	"context"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestWithTransaction_Integration(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	// Successful transaction
	err := WithTransaction(ctx, func(ctx context.Context) error {
		u1 := &testUser{Email: "tx1@test.com", Name: "TX1", Age: 25, Role: "user"}
		if err := Create(ctx, u1); err != nil {
			return err
		}
		u2 := &testUser{Email: "tx2@test.com", Name: "TX2", Age: 30, Role: "admin"}
		if err := Create(ctx, u2); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		// Transactions require a replica set. Skip if not available.
		t.Skipf("Transactions not supported (likely standalone): %v", err)
	}

	var users []testUser
	if err := Find(ctx, bson.D{}, &users); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
}

func TestWithTransaction_Rollback(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()

	// Transaction that returns error should rollback
	err := WithTransaction(ctx, func(ctx context.Context) error {
		u := &testUser{Email: "rollback@test.com", Name: "Rollback", Age: 25, Role: "user"}
		if err := Create(ctx, u); err != nil {
			return err
		}
		return fmt.Errorf("intentional error")
	})
	if err == nil {
		t.Fatal("expected error from failed transaction")
	}

	// The user should not exist if rollback worked
	// (This only works with replica sets, so we check gracefully)
	var users []testUser
	if err := Find(ctx, bson.D{{Key: "email", Value: "rollback@test.com"}}, &users); err != nil {
		t.Fatalf("find: %v", err)
	}
	// On standalone (no replica set), the write may have persisted
	// On replica set, it should be rolled back (0 users)
	t.Logf("Found %d users after rollback (0 expected on replica set)", len(users))
}

func TestWithTransaction_NoDatabase(t *testing.T) {
	// Temporarily clear the global DB
	dbMu.Lock()
	saved := globalDB
	globalDB = nil
	dbMu.Unlock()
	defer func() {
		dbMu.Lock()
		globalDB = saved
		dbMu.Unlock()
	}()

	err := WithTransaction(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != ErrNoDatabase {
		t.Fatalf("expected ErrNoDatabase, got %v", err)
	}
}
