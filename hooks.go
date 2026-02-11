package goodm

import "context"

// BeforeCreate is called before inserting a new document.
type BeforeCreate interface {
	BeforeCreate(ctx context.Context) error
}

// AfterCreate is called after inserting a new document.
type AfterCreate interface {
	AfterCreate(ctx context.Context) error
}

// BeforeSave is called before updating an existing document.
type BeforeSave interface {
	BeforeSave(ctx context.Context) error
}

// AfterSave is called after updating an existing document.
type AfterSave interface {
	AfterSave(ctx context.Context) error
}

// BeforeDelete is called before deleting a document.
type BeforeDelete interface {
	BeforeDelete(ctx context.Context) error
}

// AfterDelete is called after deleting a document.
type AfterDelete interface {
	AfterDelete(ctx context.Context) error
}
