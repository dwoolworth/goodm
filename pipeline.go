package goodm

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// PipelineOptions configures a Pipeline.
type PipelineOptions struct {
	DB *mongo.Database
}

// Pipeline is a fluent builder for MongoDB aggregation pipelines.
// It is bound to a model for collection lookup and supports chaining stages.
//
// Example:
//
//	var results []bson.M
//	err := goodm.NewPipeline(&User{}).
//	    Match(bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 21}}}}).
//	    Group(bson.D{{Key: "_id", Value: "$role"}, {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}).
//	    Sort(bson.D{{Key: "count", Value: -1}}).
//	    Limit(10).
//	    Execute(ctx, &results)
type Pipeline struct {
	model  interface{}
	stages []bson.D
	db     *mongo.Database
}

// NewPipeline creates a new aggregation pipeline builder bound to the given model.
// The model is used for schema/collection lookup (e.g. &User{}).
func NewPipeline(model interface{}, opts ...PipelineOptions) *Pipeline {
	p := &Pipeline{model: model}
	if len(opts) > 0 {
		p.db = opts[0].DB
	}
	return p
}

// Match adds a $match stage to filter documents.
func (p *Pipeline) Match(filter interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$match", Value: filter}})
	return p
}

// Group adds a $group stage for aggregation.
func (p *Pipeline) Group(group interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$group", Value: group}})
	return p
}

// Sort adds a $sort stage.
func (p *Pipeline) Sort(sort interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$sort", Value: sort}})
	return p
}

// Project adds a $project stage to reshape documents.
func (p *Pipeline) Project(projection interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$project", Value: projection}})
	return p
}

// Limit adds a $limit stage.
func (p *Pipeline) Limit(n int64) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$limit", Value: n}})
	return p
}

// Skip adds a $skip stage.
func (p *Pipeline) Skip(n int64) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$skip", Value: n}})
	return p
}

// Unwind adds a $unwind stage to deconstruct an array field.
// The field name is automatically prefixed with "$".
func (p *Pipeline) Unwind(field string) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$unwind", Value: "$" + field}})
	return p
}

// Lookup adds a $lookup stage for a left outer join.
func (p *Pipeline) Lookup(from, localField, foreignField, as string) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$lookup", Value: bson.D{
		{Key: "from", Value: from},
		{Key: "localField", Value: localField},
		{Key: "foreignField", Value: foreignField},
		{Key: "as", Value: as},
	}}})
	return p
}

// AddFields adds a $addFields stage to add computed fields.
func (p *Pipeline) AddFields(fields interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$addFields", Value: fields}})
	return p
}

// Count adds a $count stage that outputs a document with the given field
// containing the count of documents at this stage.
func (p *Pipeline) Count(field string) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$count", Value: field}})
	return p
}

// Stage appends a raw aggregation stage for operations not covered by
// the builder methods.
func (p *Pipeline) Stage(stage bson.D) *Pipeline {
	p.stages = append(p.stages, stage)
	return p
}

// Stages returns the accumulated pipeline stages. Useful for inspection or testing.
func (p *Pipeline) Stages() []bson.D {
	return p.stages
}

// Execute runs the aggregation pipeline and decodes all results into the
// provided slice pointer.
func (p *Pipeline) Execute(ctx context.Context, results interface{}) error {
	schema, err := getSchemaForModel(p.model)
	if err != nil {
		return err
	}

	db, err := getDB(p.db)
	if err != nil {
		return err
	}

	coll := db.Collection(schema.Collection)
	cursor, err := coll.Aggregate(ctx, p.stages)
	if err != nil {
		return fmt.Errorf("goodm: aggregate failed: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	if err := cursor.All(ctx, results); err != nil {
		return fmt.Errorf("goodm: aggregate decode failed: %w", err)
	}

	return nil
}

// Cursor runs the aggregation pipeline and returns a raw *mongo.Cursor
// for streaming large result sets. The caller is responsible for closing
// the cursor.
func (p *Pipeline) Cursor(ctx context.Context) (*mongo.Cursor, error) {
	schema, err := getSchemaForModel(p.model)
	if err != nil {
		return nil, err
	}

	db, err := getDB(p.db)
	if err != nil {
		return nil, err
	}

	coll := db.Collection(schema.Collection)
	cursor, err := coll.Aggregate(ctx, p.stages)
	if err != nil {
		return nil, fmt.Errorf("goodm: aggregate cursor failed: %w", err)
	}

	return cursor, nil
}
