package goodm

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// PipelineOptions configures a Pipeline.
type PipelineOptions struct {
	DB           *mongo.Database
	AllowDiskUse bool               // let stages spill to disk past the 100MB memory limit
	Collation    *options.Collation // collation for string comparisons
	Hint         interface{}        // index to use for the initial $match
	BatchSize    int32              // documents per cursor batch
}

// Pipeline is a fluent builder for MongoDB aggregation pipelines.
// It is bound to a model for collection lookup and supports chaining stages.
// Execute and Cursor run middleware under OpAggregate; middleware may replace
// OpInfo.Pipeline (for example to prepend a scoping $match).
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
	opt    PipelineOptions
}

// NewPipeline creates a new aggregation pipeline builder bound to the given model.
// The model is used for schema/collection lookup (e.g. &User{}).
func NewPipeline(model interface{}, opts ...PipelineOptions) *Pipeline {
	p := &Pipeline{model: model}
	if len(opts) > 0 {
		p.opt = opts[0]
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

// UnwindPreserve adds a $unwind stage that keeps documents whose array is
// missing, null, or empty (preserveNullAndEmptyArrays: true).
func (p *Pipeline) UnwindPreserve(field string) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$unwind", Value: bson.D{
		{Key: "path", Value: "$" + field},
		{Key: "preserveNullAndEmptyArrays", Value: true},
	}}})
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

// LookupPipeline adds a $lookup stage with a sub-pipeline and let variables,
// for joins that need conditions beyond field equality.
func (p *Pipeline) LookupPipeline(from string, let interface{}, pipeline []bson.D, as string) *Pipeline {
	stage := bson.D{{Key: "from", Value: from}}
	if let != nil {
		stage = append(stage, bson.E{Key: "let", Value: let})
	}
	stage = append(stage, bson.E{Key: "pipeline", Value: pipeline}, bson.E{Key: "as", Value: as})
	p.stages = append(p.stages, bson.D{{Key: "$lookup", Value: stage}})
	return p
}

// AddFields adds a $addFields stage to add computed fields.
func (p *Pipeline) AddFields(fields interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$addFields", Value: fields}})
	return p
}

// Set adds a $set stage (an alias of $addFields).
func (p *Pipeline) Set(fields interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$set", Value: fields}})
	return p
}

// Unset adds a $unset stage removing the named fields.
func (p *Pipeline) Unset(fields ...string) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$unset", Value: fields}})
	return p
}

// ReplaceRoot adds a $replaceRoot stage promoting the given expression
// (e.g. "$author") to the document root.
func (p *Pipeline) ReplaceRoot(newRoot interface{}) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$replaceRoot", Value: bson.D{{Key: "newRoot", Value: newRoot}}}})
	return p
}

// Facet adds a $facet stage running several sub-pipelines over the same input.
func (p *Pipeline) Facet(facets map[string][]bson.D) *Pipeline {
	doc := bson.D{}
	for name, stages := range facets {
		doc = append(doc, bson.E{Key: name, Value: stages})
	}
	p.stages = append(p.stages, bson.D{{Key: "$facet", Value: doc}})
	return p
}

// Sample adds a $sample stage selecting n random documents.
func (p *Pipeline) Sample(n int64) *Pipeline {
	p.stages = append(p.stages, bson.D{{Key: "$sample", Value: bson.D{{Key: "size", Value: n}}}})
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

func (p *Pipeline) aggregateOptions() *options.AggregateOptionsBuilder {
	opts := options.Aggregate()
	if p.opt.AllowDiskUse {
		opts.SetAllowDiskUse(true)
	}
	if p.opt.Collation != nil {
		opts.SetCollation(p.opt.Collation)
	}
	if p.opt.Hint != nil {
		opts.SetHint(p.opt.Hint)
	}
	if p.opt.BatchSize > 0 {
		opts.SetBatchSize(p.opt.BatchSize)
	}
	return opts
}

// run executes the pipeline through middleware and hands the cursor to fn.
func (p *Pipeline) run(ctx context.Context, fn func(ctx context.Context, cursor *mongo.Cursor) error) error {
	schema, err := getSchemaForModel(p.model)
	if err != nil {
		return err
	}

	info := &OpInfo{
		Operation: OpAggregate, Collection: schema.Collection,
		ModelName: schema.ModelName, Model: p.model, Pipeline: p.stages,
	}
	return runMiddleware(ctx, info, func(ctx context.Context) error {
		db, err := getDB(p.opt.DB)
		if err != nil {
			return err
		}
		coll := getCollection(db, schema)
		cursor, err := coll.Aggregate(ctx, info.Pipeline, p.aggregateOptions())
		if err != nil {
			return fmt.Errorf("goodm: aggregate failed: %w", err)
		}
		return fn(ctx, cursor)
	})
}

// Execute runs the aggregation pipeline and decodes all results into the
// provided slice pointer.
func (p *Pipeline) Execute(ctx context.Context, results interface{}) error {
	return p.run(ctx, func(ctx context.Context, cursor *mongo.Cursor) error {
		defer func() { _ = cursor.Close(ctx) }()
		if err := cursor.All(ctx, results); err != nil {
			return fmt.Errorf("goodm: aggregate decode failed: %w", err)
		}
		return nil
	})
}

// Cursor runs the aggregation pipeline and returns a raw *mongo.Cursor
// for streaming large result sets. The caller is responsible for closing
// the cursor.
func (p *Pipeline) Cursor(ctx context.Context) (*mongo.Cursor, error) {
	var out *mongo.Cursor
	err := p.run(ctx, func(ctx context.Context, cursor *mongo.Cursor) error {
		out = cursor
		return nil
	})
	return out, err
}
