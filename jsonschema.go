package goodm

import (
	"encoding/json"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// JSONSchema returns the $jsonSchema document that describes documents of
// this model, derived from the struct's Go types and goodm tags, for use as
// a MongoDB collection validator:
//
//	validator := bson.D{{Key: "$jsonSchema", Value: schema.JSONSchema()}}
//
// Enforce applies it when EnforceOptions.Validator is ValidatorApply.
//
// Each field's bsonType follows how the driver encodes its Go type (an int
// is "int" or "long", a time.Time a "date", a struct an "object" with its
// own properties); types that can be nil (pointers, slices, maps, interfaces,
// bson.D/M/Raw) also allow null, since UpdateFields stores a nil as null.
// `required` lists the field among the object's required properties, `enum`
// becomes an enum (on the elements for slices), and `min`/`max` become
// minimum/maximum for numbers, minLength/maxLength for strings,
// minItems/maxItems for arrays, and minProperties/maxProperties for maps;
// unsigned integers get minimum 0. A field typed interface{} is listed
// without constraints. Properties not declared by the struct are allowed, as
// drift detection treats them as warnings rather than errors.
//
// The server checks only that a required field is present; goodm's own
// Validate also rejects a zero value, so it remains stricter.
func (s *Schema) JSONSchema() bson.D {
	doc := objectSchema(s.Fields)
	return append(bson.D{{Key: "title", Value: s.ModelName}}, doc...)
}

// objectSchema builds the schema of an object from its fields: bsonType,
// properties, and required.
func objectSchema(fields []FieldSchema) bson.D {
	props := make(bson.D, 0, len(fields))
	var required []string
	for i := range fields {
		fs := &fields[i]
		props = append(props, bson.E{Key: fs.BSONName, Value: fieldSchema(fs)})
		if fs.Required {
			required = append(required, fs.BSONName)
		}
	}
	doc := bson.D{{Key: "bsonType", Value: "object"}}
	if len(props) > 0 {
		doc = append(doc, bson.E{Key: "properties", Value: props})
	}
	if len(required) > 0 {
		doc = append(doc, bson.E{Key: "required", Value: required})
	}
	return doc
}

// fieldSchema builds the schema of one field from its Go type and tags.
func fieldSchema(fs *FieldSchema) bson.D {
	t := fs.goType
	if t == nil {
		return bson.D{}
	}
	doc := typeSchema(t, fs)
	if constrainable(t) {
		applyEnum(&doc, fs, t)
		applyBounds(&doc, fs, t)
	}
	return doc
}

// constrainable reports whether enum and bounds from the tags describe the
// stored value: not for a type that encodes itself or one the driver
// encodes as something other than its Go kind, whose tag values are
// compared by goodm in Go terms the server would not share.
func constrainable(t reflect.Type) bool {
	elem := baseType(t)
	if elem.Kind() == reflect.Slice || elem.Kind() == reflect.Array {
		elem = baseType(elem.Elem())
	}
	if encodesItself(elem) {
		return false
	}
	switch elem {
	case reflect.TypeOf(url.URL{}), reflect.TypeOf(json.Number("")), reflect.TypeOf(bson.JavaScript("")), reflect.TypeOf(bson.Symbol("")),
		reflect.TypeOf(bson.DateTime(0)), reflect.TypeOf(bson.Timestamp{}), reflect.TypeOf(bson.Decimal128{}):
		return false
	}
	return true
}

var (
	valueMarshalerType = reflect.TypeOf((*bson.ValueMarshaler)(nil)).Elem()
	marshalerType      = reflect.TypeOf((*bson.Marshaler)(nil)).Elem()
	byteType           = reflect.TypeOf(byte(0))
)

// encodesItself reports whether values of t choose their own BSON encoding,
// which the schema cannot predict.
func encodesItself(t reflect.Type) bool {
	pt := reflect.PointerTo(t)
	return t.Implements(valueMarshalerType) || pt.Implements(valueMarshalerType) ||
		t.Implements(marshalerType) || pt.Implements(marshalerType)
}

// typeSchema maps a Go type to its bsonType (and items/properties for
// arrays and objects). fs supplies the parsed subfields of struct types.
func typeSchema(t reflect.Type, fs *FieldSchema) bson.D {
	switch t {
	case reflect.TypeOf(time.Time{}), reflect.TypeOf(bson.DateTime(0)):
		return bsonTypes("date")
	case reflect.TypeOf(bson.ObjectID{}):
		return bsonTypes("objectId")
	case reflect.TypeOf(bson.Decimal128{}):
		return bsonTypes("decimal")
	case reflect.TypeOf(bson.Timestamp{}):
		return bsonTypes("timestamp")
	case reflect.TypeOf(bson.Binary{}), reflect.TypeOf(bson.Vector{}):
		return bsonTypes("binData")
	case reflect.TypeOf(bson.Regex{}):
		return bsonTypes("regex")
	case reflect.TypeOf(bson.JavaScript("")):
		return bsonTypes("javascript")
	case reflect.TypeOf(bson.Symbol("")):
		return bsonTypes("symbol")
	case reflect.TypeOf(bson.CodeWithScope{}):
		return bsonTypes("javascriptWithScope")
	case reflect.TypeOf(bson.DBPointer{}):
		return bsonTypes("dbPointer")
	case reflect.TypeOf(bson.MinKey{}):
		return bsonTypes("minKey")
	case reflect.TypeOf(bson.MaxKey{}):
		return bsonTypes("maxKey")
	case reflect.TypeOf(bson.Null{}):
		return bsonTypes("null")
	case reflect.TypeOf(bson.Undefined{}):
		return bsonTypes("undefined")
	case reflect.TypeOf(url.URL{}):
		return bsonTypes("string")
	case reflect.TypeOf(json.Number("")):
		return bsonTypes("long", "double")
	case reflect.TypeOf(bson.D{}), reflect.TypeOf(bson.M{}), reflect.TypeOf(bson.Raw{}):
		return bsonTypes("object", "null")
	case reflect.TypeOf(bson.A{}):
		return bsonTypes("array", "null")
	case reflect.TypeOf(bson.RawValue{}):
		return bson.D{}
	}
	if t.Kind() != reflect.Pointer && encodesItself(t) {
		return bson.D{}
	}

	switch t.Kind() {
	case reflect.Pointer:
		inner := typeSchema(t.Elem(), fs)
		return allowNull(inner)
	case reflect.Interface:
		return bson.D{}
	case reflect.String:
		return bsonTypes("string")
	case reflect.Bool:
		return bsonTypes("bool")
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return bsonTypes("int")
	case reflect.Int, reflect.Int64:
		// An int is encoded as int32 when it fits; a 64-bit kind is too
		// under bson:",minsize" or a client's IntMinSize option.
		return bsonTypes("int", "long")
	case reflect.Uint8, reflect.Uint16:
		return append(bsonTypes("int"), bson.E{Key: "minimum", Value: 0.0})
	case reflect.Uint, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return append(bsonTypes("int", "long"), bson.E{Key: "minimum", Value: 0.0})
	case reflect.Float32, reflect.Float64:
		return bsonTypes("double")
	case reflect.Slice:
		if t.Elem() == byteType { // only the exact byte type is stored as binData
			return bsonTypes("binData", "null")
		}
		return allowNull(arraySchema(t.Elem(), fs))
	case reflect.Array:
		if t.Elem() == byteType {
			return bsonTypes("binData")
		}
		return arraySchema(t.Elem(), fs)
	case reflect.Map:
		doc := bsonTypes("object", "null")
		if t.Key().Kind() == reflect.String {
			if elem := typeSchema(t.Elem(), nil); len(elem) > 0 {
				doc = append(doc, bson.E{Key: "additionalProperties", Value: elem})
			}
		}
		return doc
	case reflect.Struct:
		if fs != nil && len(fs.SubFields) > 0 {
			return objectSchema(fs.SubFields)
		}
		return bsonTypes("object")
	}
	return bson.D{}
}

// arraySchema builds an array schema whose items follow elem.
func arraySchema(elem reflect.Type, fs *FieldSchema) bson.D {
	doc := bsonTypes("array")
	if items := typeSchema(elem, fs); len(items) > 0 {
		doc = append(doc, bson.E{Key: "items", Value: items})
	}
	return doc
}

// bsonTypes renders a bsonType keyword: a string for one type, an array
// for several.
func bsonTypes(types ...string) bson.D {
	if len(types) == 1 {
		return bson.D{{Key: "bsonType", Value: types[0]}}
	}
	arr := make(bson.A, len(types))
	for i, t := range types {
		arr[i] = t
	}
	return bson.D{{Key: "bsonType", Value: arr}}
}

// allowNull adds "null" to a schema's bsonType. A schema without a bsonType
// (interface{}) already allows anything.
func allowNull(doc bson.D) bson.D {
	for i, e := range doc {
		if e.Key != "bsonType" {
			continue
		}
		var types bson.A
		switch v := e.Value.(type) {
		case string:
			types = bson.A{v}
		case bson.A:
			types = append(bson.A{}, v...)
		}
		for _, t := range types {
			if t == "null" {
				return doc
			}
		}
		out := append(bson.D{}, doc...)
		out[i] = bson.E{Key: "bsonType", Value: append(types, "null")}
		return out
	}
	return doc
}

// applyEnum adds the enum tag to the schema when its values can be
// expressed in the field's type: strings as they are, numbers and bools
// parsed. Any value that does not parse disables the keyword, since the
// server would otherwise reject every document. goodm's Validate treats a
// zero value as unset and lets it through unless the field is required, and
// a field without omitempty is written as its zero value, so the zero value
// is allowed too for a scalar field that is not required, and for the
// elements of a fixed-size array (whose zero value is all zero elements).
// Validate skips nil pointers, which are written as null, so a pointer
// field or element allows null. Elements of a slice are all checked, so
// they get no zero allowance.
func applyEnum(doc *bson.D, fs *FieldSchema, t reflect.Type) {
	if len(fs.Enum) == 0 {
		return
	}
	target := *doc
	elem := baseType(t)
	allowZero := !fs.Required
	allowNil := t.Kind() == reflect.Pointer
	if elem.Kind() == reflect.Slice || elem.Kind() == reflect.Array {
		allowNil = elem.Elem().Kind() == reflect.Pointer
		allowZero = elem.Kind() == reflect.Array
		elem = baseType(elem.Elem())
		target = itemsOf(*doc)
		if target == nil {
			return
		}
	}
	values := enumValues(fs.Enum, elem)
	if values == nil {
		return
	}
	if allowZero {
		values = append(values, enumZero(elem))
	}
	if allowNil {
		values = append(values, nil)
	}
	values = dedupe(values) // the server rejects an enum with repeated values
	target = append(target, bson.E{Key: "enum", Value: values})
	if elem != baseType(t) {
		*doc = setItems(*doc, target)
	} else {
		*doc = target
	}
}

// dedupe removes repeated values from an enum list, keeping first
// occurrences in order.
func dedupe(values bson.A) bson.A {
	seen := make(map[interface{}]bool, len(values))
	out := values[:0]
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// enumZero is the zero value of an enum field's kind, in the type the enum
// values use.
func enumZero(t reflect.Type) interface{} {
	switch t.Kind() {
	case reflect.String:
		return ""
	case reflect.Bool:
		return false
	case reflect.Float32, reflect.Float64:
		return 0.0
	}
	return int64(0)
}

// enumValues converts tag values to the field's kind.
func enumValues(enum []string, t reflect.Type) bson.A {
	values := make(bson.A, 0, len(enum))
	for _, s := range enum {
		switch t.Kind() {
		case reflect.String:
			values = append(values, s)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil
			}
			values = append(values, n)
		case reflect.Float32:
			f, err := strconv.ParseFloat(s, 32)
			if err != nil {
				return nil
			}
			values = append(values, float64(float32(f))) // the stored double
		case reflect.Float64:
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil
			}
			values = append(values, f)
		case reflect.Bool:
			b, err := strconv.ParseBool(s)
			if err != nil {
				return nil
			}
			values = append(values, b)
		default:
			return nil
		}
	}
	return values
}

// applyBounds adds min/max as the keyword the field's kind supports.
func applyBounds(doc *bson.D, fs *FieldSchema, t reflect.Type) {
	if fs.Min == nil && fs.Max == nil {
		return
	}
	elem := baseType(t)
	var minKey, maxKey string
	integral := false
	switch {
	case isNumericKind(elem.Kind()):
		minKey, maxKey = "minimum", "maximum"
	case elem.Kind() == reflect.String:
		minKey, maxKey, integral = "minLength", "maxLength", true
	case (elem.Kind() == reflect.Slice || elem.Kind() == reflect.Array) && elem.Elem() != byteType:
		minKey, maxKey, integral = "minItems", "maxItems", true
	case elem.Kind() == reflect.Map:
		minKey, maxKey, integral = "minProperties", "maxProperties", true
	default:
		return
	}
	if fs.Min != nil {
		*doc = setBound(*doc, minKey, *fs.Min, integral, true)
	}
	if fs.Max != nil {
		*doc = setBound(*doc, maxKey, *fs.Max, integral, false)
	}
}

// setBound appends a bound, replacing one the type schema already set (an
// unsigned integer's minimum 0) and rounding length bounds inward to whole
// numbers so they match goodm's comparison of a length against the bound.
// A bound that is not finite constrains nothing and is left out.
func setBound(doc bson.D, key string, bound float64, integral, isMin bool) bson.D {
	if math.IsNaN(bound) || math.IsInf(bound, 0) {
		return doc
	}
	var value interface{} = bound
	if integral {
		n := math.Floor(bound)
		if isMin {
			n = math.Ceil(bound)
		}
		if n < 0 {
			n = 0
		}
		value = int64(n)
	}
	for i, e := range doc {
		if e.Key == key {
			out := append(bson.D{}, doc...)
			out[i] = bson.E{Key: key, Value: value}
			return out
		}
	}
	return append(doc, bson.E{Key: key, Value: value})
}

// baseType follows pointers to the underlying type.
func baseType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isNumericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// itemsOf returns an array schema's items document, or nil.
func itemsOf(doc bson.D) bson.D {
	for _, e := range doc {
		if e.Key == "items" {
			if items, ok := e.Value.(bson.D); ok {
				return items
			}
		}
	}
	return nil
}

// setItems returns doc with its items document replaced.
func setItems(doc bson.D, items bson.D) bson.D {
	out := append(bson.D{}, doc...)
	for i, e := range out {
		if e.Key == "items" {
			out[i] = bson.E{Key: "items", Value: items}
		}
	}
	return out
}
