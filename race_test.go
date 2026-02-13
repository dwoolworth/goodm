package goodm

import (
	"context"
	"sync"
	"testing"
)

// TestRace_RegistryReadWrite exercises concurrent reads and writes on the registry.
func TestRace_RegistryReadWrite(t *testing.T) {
	unregisterTestModels()

	var wg sync.WaitGroup

	// Writer goroutines
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registerTestModels()
			unregisterTestModels()
		}()
	}

	// Reader goroutines
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = GetAll()
			_, _ = Get("testUser")
			_, _ = Get("testProfile")
		}()
	}

	wg.Wait()
}

// TestRace_MiddlewareReadWrite exercises concurrent middleware registration and execution.
func TestRace_MiddlewareReadWrite(t *testing.T) {
	ClearMiddleware()
	defer ClearMiddleware()

	var wg sync.WaitGroup

	// Register middleware concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
				return next(ctx)
			})
		}()
	}

	// Register per-model middleware concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			UseFor("TestModel", func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
				return next(ctx)
			})
		}()
	}

	// Execute middleware concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runMiddleware(context.Background(), &OpInfo{
				Operation: OpFind, ModelName: "TestModel",
			}, func(ctx context.Context) error {
				return nil
			})
		}()
	}

	wg.Wait()
}

// TestRace_ConcurrentValidation exercises concurrent validation calls.
func TestRace_ConcurrentValidation(t *testing.T) {
	schema := &Schema{
		Fields: []FieldSchema{
			{Name: "Name", BSONName: "name", Required: true, Min: intPtr(2), Max: intPtr(50)},
			{Name: "Role", BSONName: "role", Enum: []string{"admin", "user"}},
		},
	}

	type model struct {
		Name string
		Role string
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Validate(&model{Name: "Alice", Role: "admin"}, schema)
			_ = Validate(&model{Name: "", Role: "invalid"}, schema)
		}()
	}

	wg.Wait()
}
