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
}
