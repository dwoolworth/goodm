package goodm

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type testProduct struct {
	Model `bson:",inline"`
	Name  string `bson:"name"`
}

type testCarrier struct {
	Model `bson:",inline"`
	Name  string `bson:"name"`
}

type testLineItem struct {
	ProductID bson.ObjectID `bson:"product" goodm:"ref=test_products"`
	Qty       int           `bson:"qty"`
}

type testShipTo struct {
	CarrierID bson.ObjectID   `bson:"carrier" goodm:"ref=test_carriers"`
	TagIDs    []bson.ObjectID `bson:"tags"    goodm:"ref=test_tags"`
	Backup    *bson.ObjectID  `bson:"backup"  goodm:"ref=test_carriers"`
}

type testInvoice struct {
	Model    `bson:",inline"`
	Name     string          `bson:"name"`
	Items    []testLineItem  `bson:"items"`
	PtrItems []*testLineItem `bson:"ptr_items"`
	Ship     testShipTo      `bson:"ship"`
	AltShip  *testShipTo     `bson:"alt_ship"`
}

func registerInvoiceModels(t *testing.T) {
	t.Helper()
	for _, m := range []struct {
		model interface{}
		coll  string
	}{{&testProduct{}, "test_products"}, {&testCarrier{}, "test_carriers"}, {&testInvoice{}, "test_invoices"}} {
		if err := Register(m.model, m.coll); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		Unregister(&testProduct{})
		Unregister(&testCarrier{})
		Unregister(&testInvoice{})
	})
}

func TestPopulateNestedPaths(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerInvoiceModels(t)

	products := []*testProduct{{Name: "p1"}, {Name: "p2"}, {Name: "p3"}}
	for _, p := range products {
		if err := Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	carrier := &testCarrier{Name: "c1"}
	backup := &testCarrier{Name: "c2"}
	tags := []*testTag{{Label: "t1"}, {Label: "t2"}}
	for _, m := range []interface{}{carrier, backup, tags[0], tags[1]} {
		if err := Create(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	inv := &testInvoice{
		Name: "inv",
		Items: []testLineItem{
			{ProductID: products[0].ID, Qty: 1},
			{ProductID: products[1].ID, Qty: 2},
			{ProductID: products[0].ID, Qty: 3}, // duplicate id
			{Qty: 4},                            // zero id
		},
		PtrItems: []*testLineItem{nil, {ProductID: products[2].ID}},
		Ship:     testShipTo{CarrierID: carrier.ID, TagIDs: []bson.ObjectID{tags[0].ID, tags[1].ID}, Backup: &backup.ID},
	}
	if err := Create(ctx, inv); err != nil {
		t.Fatal(err)
	}

	// A ref through a slice fans out into a slice target, deduplicated.
	var got []testProduct
	if err := Populate(ctx, inv, Refs{"items.product": &got}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("items.product populated %d products, want 2", len(got))
	}

	// A single target for a fan-out path is an error.
	err := Populate(ctx, inv, Refs{"items.product": &testProduct{}})
	if err == nil || !strings.Contains(err.Error(), "several ids") {
		t.Errorf("fan-out into struct = %v", err)
	}

	// An element index names one ref.
	var one testProduct
	if err := Populate(ctx, inv, Refs{"items.1.product": &one}); err != nil {
		t.Fatal(err)
	}
	if one.ID != products[1].ID {
		t.Errorf("items.1.product = %v", one.Name)
	}
	// Past the end, or a zero id: untouched, no error.
	untouched := testProduct{Name: "keep"}
	if err := Populate(ctx, inv, Refs{"items.9.product": &untouched, "items.3.product": &untouched}); err != nil || untouched.Name != "keep" {
		t.Errorf("out of range: %v %q", err, untouched.Name)
	}

	// Nil pointer elements are skipped.
	got = nil
	if err := Populate(ctx, inv, Refs{"ptr_items.product": &got}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != products[2].ID {
		t.Errorf("ptr_items.product = %+v", got)
	}

	// Refs inside a struct, a pointer struct (nil and set), and a
	// pointer-to-ObjectID leaf.
	var c, b testCarrier
	var tg []testTag
	if err := Populate(ctx, inv, Refs{"ship.carrier": &c, "ship.backup": &b, "ship.tags": &tg}); err != nil {
		t.Fatal(err)
	}
	if c.ID != carrier.ID || b.ID != backup.ID || len(tg) != 2 {
		t.Errorf("ship refs: %v %v %d", c.Name, b.Name, len(tg))
	}
	var oneTag testTag
	if err := Populate(ctx, inv, Refs{"ship.tags.1": &oneTag}); err != nil || oneTag.ID != tags[1].ID {
		t.Errorf("ship.tags.1 = %v %v", oneTag.Label, err)
	}
	alt := testCarrier{Name: "keep"}
	if err := Populate(ctx, inv, Refs{"alt_ship.carrier": &alt}); err != nil || alt.Name != "keep" {
		t.Errorf("nil subdocument: %v %q", err, alt.Name)
	}
	inv.AltShip = &testShipTo{CarrierID: backup.ID}
	if err := Populate(ctx, inv, Refs{"alt_ship.carrier": &alt}); err != nil || alt.ID != backup.ID {
		t.Errorf("set subdocument: %v %v", err, alt.Name)
	}

	// A single ref may also be decoded into a slice target.
	var cs []testCarrier
	if err := Populate(ctx, inv, Refs{"ship.carrier": &cs}); err != nil || len(cs) != 1 {
		t.Errorf("single into slice: %v %d", err, len(cs))
	}

	// Path errors.
	for _, bad := range []string{"nope.product", "items.nope", "items.qty", "name.x", "items.-1.product", "ship.carrier.x", "items.0.1.product"} {
		if err := Populate(ctx, inv, Refs{bad: &got}); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	if err := Populate(ctx, inv, Refs{"ship.carrier": nil}); err == nil {
		t.Error("nil target accepted")
	}
	if err := Populate(ctx, inv, Refs{"ship.carrier": testCarrier{}}); err == nil {
		t.Error("non-pointer target accepted")
	}
}

func TestBatchPopulateNestedPaths(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerInvoiceModels(t)

	products := []*testProduct{{Name: "p1"}, {Name: "p2"}, {Name: "p3"}}
	for _, p := range products {
		if err := Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	invoices := []*testInvoice{
		{Name: "a", Items: []testLineItem{{ProductID: products[0].ID}, {ProductID: products[1].ID}}},
		nil,
		{Name: "b", Items: []testLineItem{{ProductID: products[1].ID}}, Ship: testShipTo{CarrierID: products[2].ID}},
	}
	var got []testProduct
	if err := BatchPopulate(ctx, invoices, "items.product", &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("batch items.product = %d products, want 2", len(got))
	}
	got = nil
	if err := BatchPopulate(ctx, invoices, "items.0.product", &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("batch items.0.product = %d, want 2", len(got))
	}
	if err := BatchPopulate(ctx, invoices, "items.qty", &got); err == nil {
		t.Error("no ref tag accepted")
	}
	// Value slices work too.
	vals := []testInvoice{*invoices[0]}
	got = nil
	if err := BatchPopulate(ctx, vals, "items.product", &got); err != nil || len(got) != 2 {
		t.Errorf("value slice: %v %d", err, len(got))
	}
}

type reviewNode struct {
	Ref  bson.ObjectID `bson:"ref" goodm:"ref=test_products"`
	Kids []reviewNode  `bson:"kids"`
}

type testTree struct {
	Model `bson:",inline"`
	Root  reviewNode      `bson:"root"`
	Pair  [2]testLineItem `bson:"pair"`
}

func TestPopulatePathContract(t *testing.T) {
	ctx, _, cleanup := setupTestDB(t)
	defer cleanup()
	registerInvoiceModels(t)
	if err := Register(&testTree{}, "test_trees"); err != nil {
		t.Fatal(err)
	}
	defer Unregister(&testTree{})

	p := &testProduct{Name: "p"}
	if err := Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	inv := &testInvoice{Name: "i", Ship: testShipTo{CarrierID: p.ID}}
	if err := Create(ctx, inv); err != nil {
		t.Fatal(err)
	}

	// A slice or nil model is an error, not a silent no-op.
	var c testCarrier
	if err := Populate(ctx, []*testInvoice{inv}, Refs{"ship.carrier": &c}); err == nil {
		t.Error("slice model accepted")
	}
	if err := Populate(ctx, (*testInvoice)(nil), Refs{"ship.carrier": &c}); err == nil {
		t.Error("nil model accepted")
	}

	// An ObjectID is not indexable.
	for _, bad := range []string{"ship.carrier.0", "ship.backup.5", "name.0"} {
		if err := Populate(ctx, inv, Refs{bad: &c}); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}

	// The target type is judged by the path, not by what the model holds:
	// a struct target for a fan-out path fails even through a nil pointer
	// or an empty slice.
	var one testTag
	if err := Populate(ctx, inv, Refs{"alt_ship.tags": &one}); err == nil || !strings.Contains(err.Error(), "several ids") {
		t.Errorf("fan-out through nil pointer with struct target = %v", err)
	}
	if err := Populate(ctx, inv, Refs{"ship.tags": &one}); err == nil || !strings.Contains(err.Error(), "several ids") {
		t.Errorf("empty array ref with struct target = %v", err)
	}
	if err := Populate(ctx, inv, Refs{"items.product": &testProduct{}}); err == nil {
		t.Error("fan-out through empty slice with struct target accepted")
	}
	// A nil target is an error even for a zero ref.
	if err := Populate(ctx, inv, Refs{"ship.backup": nil}); err == nil {
		t.Error("nil target accepted for a zero ref")
	}
	// Error text names the field before an index.
	err := Populate(ctx, inv, Refs{"ship.tags.0.1": &one})
	if err == nil || !strings.Contains(err.Error(), `field "tags"`) {
		t.Errorf("index error text = %v", err)
	}

	// A ref inside a self-recursive subdocument type is reachable.
	tree := &testTree{Root: reviewNode{Kids: []reviewNode{{Ref: p.ID}, {Kids: []reviewNode{{Ref: p.ID}}}}}}
	if err := Create(ctx, tree); err != nil {
		t.Fatal(err)
	}
	var got []testProduct
	if err := Populate(ctx, tree, Refs{"root.kids.ref": &got}); err != nil || len(got) != 1 {
		t.Errorf("recursive path: %v %d", err, len(got))
	}
	got = nil
	if err := Populate(ctx, tree, Refs{"root.kids.1.kids.0.ref": &got}); err != nil || len(got) != 1 {
		t.Errorf("deep recursive path: %v %d", err, len(got))
	}
	var single testProduct
	if err := Populate(ctx, tree, Refs{"root.kids.1.kids.0.ref": &single}); err != nil || single.ID != p.ID {
		t.Errorf("deep indexed path: %v %v", err, single.ID)
	}
	if err := BatchPopulate(ctx, []*testTree{tree}, "root.kids.kids.ref", &got); err != nil || len(got) != 1 {
		t.Errorf("batch recursive: %v %d", err, len(got))
	}

	// A fixed-size array of subdocuments is not a path element, as Register
	// does not treat it as a subdocument list.
	for _, bad := range []string{"pair.product", "pair.0.product"} {
		if err := Populate(ctx, tree, Refs{bad: &got}); err == nil {
			t.Errorf("array path %q accepted", bad)
		}
	}
}
