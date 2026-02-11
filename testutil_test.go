package goodm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// --- test models ---

type testUser struct {
	Model     `bson:",inline"`
	Email     string        `bson:"email"   goodm:"unique,required"`
	Name      string        `bson:"name"    goodm:"required,immutable"`
	Age       int           `bson:"age"     goodm:"min=0,max=200"`
	Role      string        `bson:"role"    goodm:"enum=admin|user,default=user"`
	ProfileID bson.ObjectID `bson:"profile" goodm:"ref=test_profiles"`
}

type testProfile struct {
	Model `bson:",inline"`
	Bio   string `bson:"bio"`
}

type testHookUser struct {
	Model  `bson:",inline"`
	Email  string `bson:"email" goodm:"required"`
	Name   string `bson:"name"  goodm:"required"`
	Events []string
}

func (u *testHookUser) BeforeCreate(ctx context.Context) error {
	u.Events = append(u.Events, "before_create")
	return nil
}
func (u *testHookUser) AfterCreate(ctx context.Context) error {
	u.Events = append(u.Events, "after_create")
	return nil
}
func (u *testHookUser) BeforeSave(ctx context.Context) error {
	u.Events = append(u.Events, "before_save")
	return nil
}
func (u *testHookUser) AfterSave(ctx context.Context) error {
	u.Events = append(u.Events, "after_save")
	return nil
}
func (u *testHookUser) BeforeDelete(ctx context.Context) error {
	u.Events = append(u.Events, "before_delete")
	return nil
}
func (u *testHookUser) AfterDelete(ctx context.Context) error {
	u.Events = append(u.Events, "after_delete")
	return nil
}

// --- test DB setup ---

func setupTestDB(t *testing.T) (context.Context, *mongo.Database, func()) {
	t.Helper()
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}

	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Skipf("MongoDB not available: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("MongoDB not available: %v", err)
	}

	dbName := fmt.Sprintf("goodm_test_%d", time.Now().UnixNano())
	db := client.Database(dbName)

	// Verify we can actually perform operations (auth check)
	testColl := db.Collection("_goodm_auth_check")
	if _, err := testColl.InsertOne(ctx, bson.D{{Key: "test", Value: true}}); err != nil {
		_ = db.Drop(ctx)
		t.Skipf("MongoDB not writable (auth required?): %v", err)
	}
	_ = testColl.Drop(ctx)

	// Set global DB
	dbMu.Lock()
	globalDB = db
	dbMu.Unlock()

	// Register test models
	registerTestModels()

	cleanup := func() {
		_ = db.Drop(ctx)
		dbMu.Lock()
		globalDB = nil
		dbMu.Unlock()
		unregisterTestModels()
		ClearMiddleware()
	}

	return ctx, db, cleanup
}

func registerTestModels() {
	_ = Register(&testUser{}, "test_users")
	_ = Register(&testProfile{}, "test_profiles")
	_ = Register(&testHookUser{}, "test_hook_users")
}

func unregisterTestModels() {
	registryMu.Lock()
	delete(registry, "testUser")
	delete(registry, "testProfile")
	delete(registry, "testHookUser")
	registryMu.Unlock()
}
