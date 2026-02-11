package goodm

import (
	"context"
	"testing"
)

func TestRunMiddleware_NoMiddleware(t *testing.T) {
	ClearMiddleware()
	defer ClearMiddleware()

	called := false
	err := runMiddleware(context.Background(), &OpInfo{
		Operation: OpCreate, ModelName: "Test",
	}, func(ctx context.Context) error {
		called = true
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("inner function was not called")
	}
}

func TestRunMiddleware_GlobalOrder(t *testing.T) {
	ClearMiddleware()
	defer ClearMiddleware()

	var order []int
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		order = append(order, 1)
		err := next(ctx)
		order = append(order, 4)
		return err
	})
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		order = append(order, 2)
		err := next(ctx)
		order = append(order, 3)
		return err
	})

	err := runMiddleware(context.Background(), &OpInfo{
		Operation: OpCreate, ModelName: "Foo",
	}, func(ctx context.Context) error {
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []int{1, 2, 3, 4}
	if len(order) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Fatalf("expected %v, got %v", expected, order)
		}
	}
}

func TestRunMiddleware_PerModel(t *testing.T) {
	ClearMiddleware()
	defer ClearMiddleware()

	var globalCalled, modelCalled bool
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		globalCalled = true
		return next(ctx)
	})
	UseFor("TargetModel", func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		modelCalled = true
		return next(ctx)
	})

	// Call with matching model name
	_ = runMiddleware(context.Background(), &OpInfo{
		Operation: OpCreate, ModelName: "TargetModel",
	}, func(ctx context.Context) error { return nil })

	if !globalCalled {
		t.Fatal("global middleware was not called")
	}
	if !modelCalled {
		t.Fatal("per-model middleware was not called")
	}

	// Call with non-matching model name
	globalCalled = false
	modelCalled = false
	_ = runMiddleware(context.Background(), &OpInfo{
		Operation: OpCreate, ModelName: "OtherModel",
	}, func(ctx context.Context) error { return nil })

	if !globalCalled {
		t.Fatal("global middleware was not called for other model")
	}
	if modelCalled {
		t.Fatal("per-model middleware should not fire for other model")
	}
}

func TestRunMiddleware_Abort(t *testing.T) {
	ClearMiddleware()
	defer ClearMiddleware()

	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		return context.Canceled // abort, don't call next
	})

	innerCalled := false
	err := runMiddleware(context.Background(), &OpInfo{
		Operation: OpCreate, ModelName: "Test",
	}, func(ctx context.Context) error {
		innerCalled = true
		return nil
	})

	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if innerCalled {
		t.Fatal("inner function should not have been called")
	}
}

func TestRunMiddleware_OpInfo(t *testing.T) {
	ClearMiddleware()
	defer ClearMiddleware()

	var captured *OpInfo
	Use(func(ctx context.Context, op *OpInfo, next func(context.Context) error) error {
		captured = op
		return next(ctx)
	})

	_ = runMiddleware(context.Background(), &OpInfo{
		Operation: OpDelete, Collection: "users", ModelName: "User",
	}, func(ctx context.Context) error { return nil })

	if captured == nil {
		t.Fatal("OpInfo was not captured")
	}
	if captured.Operation != OpDelete {
		t.Fatalf("expected OpDelete, got %v", captured.Operation)
	}
	if captured.Collection != "users" {
		t.Fatalf("expected 'users', got %v", captured.Collection)
	}
	if captured.ModelName != "User" {
		t.Fatalf("expected 'User', got %v", captured.ModelName)
	}
}
