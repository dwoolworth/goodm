package goodm

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Model is the base struct that all goodm models should embed.
// It provides automatic ID generation and timestamp management.
type Model struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	CreatedAt time.Time     `bson:"created_at"`
	UpdatedAt time.Time     `bson:"updated_at"`
}
