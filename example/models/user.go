package models

import (
	"context"
	"time"

	"github.com/dwoolworth/goodm"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Role represents a user role.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
	RoleMod   Role = "mod"
)

// User is an example model demonstrating goodm schema features.
type User struct {
	goodm.Model `bson:",inline"`
	Email       string        `bson:"email"    goodm:"unique,index,required"`
	Name        string        `bson:"name"     goodm:"required,immutable"`
	Role        Role          `bson:"role"     goodm:"enum=admin|user|mod,default=user"`
	Age         int           `bson:"age"      goodm:"min=13,max=120"`
	Profile     bson.ObjectID `bson:"profile"  goodm:"ref=profiles"`
	Verified    bool          `bson:"verified" goodm:"default=false"`
}

// Indexes returns compound indexes for the User model.
func (u *User) Indexes() []goodm.CompoundIndex {
	return []goodm.CompoundIndex{
		goodm.NewCompoundIndex("email", "role"),
	}
}

// BeforeCreate sets default timestamps.
func (u *User) BeforeCreate(ctx context.Context) error {
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	return nil
}

func init() {
	if err := goodm.Register(&User{}, "users"); err != nil {
		panic(err)
	}
}
