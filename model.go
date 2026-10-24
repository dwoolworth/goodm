package goodm

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Model is the base struct that all goodm models should embed.
// It provides automatic ID generation, timestamp management, and optimistic concurrency control.
type Model struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	CreatedAt time.Time     `bson:"created_at"`
	UpdatedAt time.Time     `bson:"updated_at"`
	Version   int           `bson:"__v"`

	// base is the document as last loaded from or written to the database
	// through goodm. Update's WithRetry uses it as the third side of its
	// 3-way merge: the caller's changes are whatever differs from it. It is
	// never encoded and travels with struct copies.
	base bson.Raw
}

// setBase records the last known stored state of the document.
func (m *Model) setBase(raw bson.Raw) {
	if raw == nil {
		m.base = nil
		return
	}
	m.base = append(bson.Raw(nil), raw...)
}

// baseSnapshot returns the last known stored state, or nil if the model was
// never loaded or saved through goodm.
func (m *Model) baseSnapshot() bson.Raw { return m.base }
