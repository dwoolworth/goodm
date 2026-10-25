package goodm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ValidatorPolicy controls whether Enforce manages a collection's validator
// (its $jsonSchema, validation level, and validation action).
type ValidatorPolicy int

const (
	// ValidatorIgnore leaves collection validators alone. This is the default.
	ValidatorIgnore ValidatorPolicy = iota
	// ValidatorApply sets each collection's validator to the schema's
	// JSONSchema with the configured level and action, replacing whatever
	// validator the collection had, and creates the collection if it does
	// not exist. A collection whose validator already matches is untouched.
	ValidatorApply
	// ValidatorVerify returns an EnforcementError when a collection's
	// validator, level, or action differs from the schema, and never changes
	// the database.
	ValidatorVerify
)

// Validation level and action values accepted by MongoDB.
const (
	ValidationLevelStrict   = "strict"   // validate every insert and update (the default)
	ValidationLevelModerate = "moderate" // validate inserts, and updates to documents that already pass
	ValidationActionError   = "error"    // reject documents that fail (the default)
	ValidationActionWarn    = "warn"     // log failures and accept the write
)

// collectionValidation is what the server reports about a collection's
// validator.
type collectionValidation struct {
	exists    bool
	validator bson.Raw
	level     string
	action    string
}

// readCollectionValidation looks a collection up with listCollections.
func readCollectionValidation(ctx context.Context, db *mongo.Database, name string) (collectionValidation, error) {
	cursor, err := db.ListCollections(ctx, bson.D{{Key: "name", Value: name}})
	if err != nil {
		return collectionValidation{}, err
	}
	defer func() { _ = cursor.Close(ctx) }()
	if !cursor.Next(ctx) {
		return collectionValidation{}, cursor.Err()
	}
	var info struct {
		Options struct {
			Validator        bson.Raw `bson:"validator"`
			ValidationLevel  string   `bson:"validationLevel"`
			ValidationAction string   `bson:"validationAction"`
		} `bson:"options"`
	}
	if err := cursor.Decode(&info); err != nil {
		return collectionValidation{}, err
	}
	return collectionValidation{
		exists:    true,
		validator: info.Options.Validator,
		level:     info.Options.ValidationLevel,
		action:    info.Options.ValidationAction,
	}, nil
}

// enforceValidator brings one collection's validator in line with the
// schema per the policy. shared names the other models registered for the
// same collection, whose fields the schema does not describe.
func enforceValidator(ctx context.Context, db *mongo.Database, schema *Schema, opt EnforceOptions, shared []*Schema) error {
	if opt.Validator == ValidatorIgnore {
		return nil
	}
	if len(shared) > 0 {
		names := make([]string, 0, len(shared))
		for _, s := range shared {
			names = append(names, s.QualifiedName())
		}
		return &EnforcementError{
			Collection: schema.Collection,
			Message:    fmt.Sprintf("cannot derive a validator for %s from model %s: the collection is also registered for %s", schema.Collection, schema.QualifiedName(), strings.Join(names, ", ")),
		}
	}

	level, action := opt.ValidationLevel, opt.ValidationAction
	if level == "" {
		level = ValidationLevelStrict
	}
	if action == "" {
		action = ValidationActionError
	}
	validator := bson.D{{Key: "$jsonSchema", Value: schema.JSONSchema()}}

	current, err := readCollectionValidation(ctx, db, schema.Collection)
	if err != nil {
		return &EnforcementError{Collection: schema.Collection, Message: fmt.Sprintf("failed to read collection options: %v", err)}
	}
	mismatch := describeValidatorMismatch(current, validator, level, action)
	if mismatch == "" {
		return nil
	}
	if opt.Validator == ValidatorVerify {
		return &EnforcementError{Collection: schema.Collection, Message: mismatch + " (use ValidatorApply to set it)"}
	}

	if !current.exists {
		createOpts := options.CreateCollection().SetValidator(validator).SetValidationLevel(level).SetValidationAction(action)
		err := db.CreateCollection(ctx, schema.Collection, createOpts)
		if err == nil {
			return nil
		}
		var cmdErr mongo.CommandError
		if !errors.As(err, &cmdErr) || cmdErr.Code != codeNamespaceExists {
			return &EnforcementError{Collection: schema.Collection, Message: fmt.Sprintf("failed to create collection with validator: %v", err)}
		}
		// Created by someone else since the lookup; fall through to collMod.
	}
	cmd := bson.D{
		{Key: "collMod", Value: schema.Collection},
		{Key: "validator", Value: validator},
		{Key: "validationLevel", Value: level},
		{Key: "validationAction", Value: action},
	}
	if err := db.RunCommand(ctx, cmd).Err(); err != nil {
		return &EnforcementError{Collection: schema.Collection, Message: fmt.Sprintf("failed to set validator: %v", err)}
	}
	return nil
}

// codeNamespaceExists is the server error for creating a collection that
// already exists.
const codeNamespaceExists = 48

// describeValidatorMismatch says how the collection's validation differs
// from what the schema wants, or "" if it matches. A missing level or
// action means the server default.
func describeValidatorMismatch(current collectionValidation, validator bson.D, level, action string) string {
	if !current.exists {
		return "collection does not exist, so it has no validator"
	}
	if len(current.validator) == 0 {
		return "collection has no validator"
	}
	equal, err := documentsEquivalent(current.validator, validator)
	if err != nil {
		return fmt.Sprintf("collection validator could not be compared: %v", err)
	}
	if !equal {
		return "collection validator differs from the schema"
	}
	if cur := orDefault(current.level, ValidationLevelStrict); cur != level {
		return fmt.Sprintf("collection validation level is %q (schema expects %q)", cur, level)
	}
	if cur := orDefault(current.action, ValidationActionError); cur != action {
		return fmt.Sprintf("collection validation action is %q (schema expects %q)", cur, action)
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// documentsEquivalent compares two documents by meaning: keys in any order
// at every level, arrays in order, and numbers by value regardless of
// width.
func documentsEquivalent(a, b interface{}) (bool, error) {
	ca, err := canonicalDocument(a)
	if err != nil {
		return false, err
	}
	cb, err := canonicalDocument(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ca, cb), nil
}

// canonicalDocument marshals a document, decodes it to generic values, sorts
// the keys of every nested document, widens integers to doubles, and
// marshals the result.
func canonicalDocument(v interface{}) ([]byte, error) {
	raw, err := bson.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc bson.D
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return bson.Marshal(canonicalize(doc))
}

func canonicalize(v interface{}) interface{} {
	switch x := v.(type) {
	case bson.D:
		out := make(bson.D, len(x))
		for i, e := range x {
			out[i] = bson.E{Key: e.Key, Value: canonicalize(e.Value)}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return out
	case bson.A:
		out := make(bson.A, len(x))
		for i, e := range x {
			out[i] = canonicalize(e)
		}
		return out
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	}
	return v
}
