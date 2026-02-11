package goodm

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPipeline_Match(t *testing.T) {
	p := NewPipeline(&testUser{})
	p.Match(bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}})

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0][0].Key != "$match" {
		t.Fatalf("expected $match, got %s", stages[0][0].Key)
	}
}

func TestPipeline_Chaining(t *testing.T) {
	p := NewPipeline(&testUser{}).
		Match(bson.D{{Key: "role", Value: "admin"}}).
		Group(bson.D{
			{Key: "_id", Value: "$role"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}).
		Sort(bson.D{{Key: "count", Value: -1}}).
		Limit(10).
		Skip(5)

	stages := p.Stages()
	if len(stages) != 5 {
		t.Fatalf("expected 5 stages, got %d", len(stages))
	}

	expectedKeys := []string{"$match", "$group", "$sort", "$limit", "$skip"}
	for i, key := range expectedKeys {
		if stages[i][0].Key != key {
			t.Errorf("stage %d: expected %s, got %s", i, key, stages[i][0].Key)
		}
	}
}

func TestPipeline_Project(t *testing.T) {
	p := NewPipeline(&testUser{}).
		Project(bson.D{
			{Key: "email", Value: 1},
			{Key: "name", Value: 1},
			{Key: "_id", Value: 0},
		})

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0][0].Key != "$project" {
		t.Fatalf("expected $project, got %s", stages[0][0].Key)
	}
}

func TestPipeline_Unwind(t *testing.T) {
	p := NewPipeline(&testUser{}).Unwind("tags")

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0][0].Key != "$unwind" {
		t.Fatalf("expected $unwind, got %s", stages[0][0].Key)
	}
	if stages[0][0].Value != "$tags" {
		t.Fatalf("expected $tags, got %v", stages[0][0].Value)
	}
}

func TestPipeline_Lookup(t *testing.T) {
	p := NewPipeline(&testUser{}).
		Lookup("profiles", "profile", "_id", "profile_data")

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0][0].Key != "$lookup" {
		t.Fatalf("expected $lookup, got %s", stages[0][0].Key)
	}
	lookupDoc := stages[0][0].Value.(bson.D)
	if len(lookupDoc) != 4 {
		t.Fatalf("expected 4 lookup fields, got %d", len(lookupDoc))
	}
}

func TestPipeline_AddFields(t *testing.T) {
	p := NewPipeline(&testUser{}).
		AddFields(bson.D{{Key: "fullName", Value: "$name"}})

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0][0].Key != "$addFields" {
		t.Fatalf("expected $addFields, got %s", stages[0][0].Key)
	}
}

func TestPipeline_Count(t *testing.T) {
	p := NewPipeline(&testUser{}).
		Match(bson.D{{Key: "role", Value: "admin"}}).
		Count("total")

	stages := p.Stages()
	if len(stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(stages))
	}
	if stages[1][0].Key != "$count" {
		t.Fatalf("expected $count, got %s", stages[1][0].Key)
	}
	if stages[1][0].Value != "total" {
		t.Fatalf("expected 'total', got %v", stages[1][0].Value)
	}
}

func TestPipeline_RawStage(t *testing.T) {
	p := NewPipeline(&testUser{}).
		Stage(bson.D{{Key: "$sample", Value: bson.D{{Key: "size", Value: 5}}}})

	stages := p.Stages()
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(stages))
	}
	if stages[0][0].Key != "$sample" {
		t.Fatalf("expected $sample, got %s", stages[0][0].Key)
	}
}

func TestPipeline_Empty(t *testing.T) {
	p := NewPipeline(&testUser{})
	stages := p.Stages()
	if stages != nil {
		t.Fatalf("expected nil stages for empty pipeline, got %v", stages)
	}
}
