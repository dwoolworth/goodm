package models

import (
	"context"
	"time"

	"github.com/dwoolworth/goodm"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Status represents a post publication status.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
	StatusArchived  Status = "archived"
)

// Post is an example model for blog posts.
type Post struct {
	goodm.Model `bson:",inline"`
	Title       string        `bson:"title"    goodm:"required"`
	Body        string        `bson:"body"     goodm:"required"`
	Author      bson.ObjectID `bson:"author"   goodm:"required,index,ref=users"`
	Status      Status        `bson:"status"   goodm:"enum=draft|published|archived,default=draft"`
	Tags        []string      `bson:"tags"     goodm:"index"`
}

// Indexes returns compound indexes for the Post model.
func (p *Post) Indexes() []goodm.CompoundIndex {
	return []goodm.CompoundIndex{
		goodm.NewCompoundIndex("author", "status"),
		goodm.NewUniqueCompoundIndex("title", "author"),
	}
}

// BeforeCreate sets timestamps on new posts.
func (p *Post) BeforeCreate(ctx context.Context) error {
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	return nil
}

// BeforeSave updates the UpdatedAt timestamp.
func (p *Post) BeforeSave(ctx context.Context) error {
	p.UpdatedAt = time.Now()
	return nil
}

func init() {
	if err := goodm.Register(&Post{}, "posts"); err != nil {
		panic(err)
	}
}
