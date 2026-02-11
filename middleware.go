package goodm

import (
	"context"
	"sync"
)

// OpType identifies the kind of CRUD operation being performed.
type OpType string

const (
	OpCreate     OpType = "create"
	OpFind       OpType = "find"
	OpUpdate     OpType = "update"
	OpDelete     OpType = "delete"
	OpCreateMany OpType = "create_many"
	OpUpdateMany OpType = "update_many"
	OpDeleteMany OpType = "delete_many"
)

// OpInfo provides context about the current operation to middleware.
type OpInfo struct {
	Operation  OpType
	Collection string
	ModelName  string
	Model      interface{} // the model being operated on, or nil
	Filter     interface{} // the query filter, if applicable
}

// MiddlewareFunc is a function that wraps a CRUD operation.
// Call next(ctx) to continue the middleware chain, or return an error to abort.
// The context can be modified before passing to next (e.g. for tracing).
type MiddlewareFunc func(ctx context.Context, op *OpInfo, next func(context.Context) error) error

var (
	mwMu    sync.RWMutex
	globalMW []MiddlewareFunc
	modelMW  map[string][]MiddlewareFunc
)

// Use registers global middleware applied to all CRUD operations.
// Middleware executes in the order registered: global first, then per-model.
func Use(fns ...MiddlewareFunc) {
	mwMu.Lock()
	defer mwMu.Unlock()
	globalMW = append(globalMW, fns...)
}

// UseFor registers middleware for a specific model name (the Go struct name).
// Per-model middleware executes after global middleware.
func UseFor(modelName string, fns ...MiddlewareFunc) {
	mwMu.Lock()
	defer mwMu.Unlock()
	if modelMW == nil {
		modelMW = make(map[string][]MiddlewareFunc)
	}
	modelMW[modelName] = append(modelMW[modelName], fns...)
}

// ClearMiddleware removes all registered middleware. Useful for testing.
func ClearMiddleware() {
	mwMu.Lock()
	defer mwMu.Unlock()
	globalMW = nil
	modelMW = nil
}

// runMiddleware builds and executes the middleware chain for an operation.
// If no middleware is registered, fn is called directly.
func runMiddleware(ctx context.Context, info *OpInfo, fn func(context.Context) error) error {
	mwMu.RLock()
	chain := make([]MiddlewareFunc, 0, len(globalMW))
	chain = append(chain, globalMW...)
	if m, ok := modelMW[info.ModelName]; ok {
		chain = append(chain, m...)
	}
	mwMu.RUnlock()

	if len(chain) == 0 {
		return fn(ctx)
	}

	// Build chain from outermost to innermost, with fn as the final handler.
	var build func(int) func(context.Context) error
	build = func(i int) func(context.Context) error {
		if i == len(chain) {
			return fn
		}
		return func(ctx context.Context) error {
			return chain[i](ctx, info, build(i+1))
		}
	}

	return build(0)(ctx)
}
