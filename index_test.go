package goodm

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// testRichIndexes exercises every index feature expressible in tags and Indexes().
type testRichIndexes struct {
	Model     `bson:",inline"`
	Tenant    string    `bson:"tenant"     goodm:"index=hashed"`
	Slug      string    `bson:"slug"       goodm:"unique,sparse"`
	Title     string    `bson:"title"      goodm:"index=text"`
	ExpiresAt time.Time `bson:"expires_at" goodm:"ttl=1h"`
	Score     int       `bson:"score"      goodm:"index=-1"`
	Address   struct {
		City string `bson:"city" goodm:"index"`
	} `bson:"address"`
}

func (m *testRichIndexes) Indexes() []CompoundIndex {
	return []CompoundIndex{
		NewCompoundIndex("tenant", "-created_at"),
		{
			Fields:        []string{"slug", "tenant"},
			Unique:        true,
			Name:          "slug_per_tenant",
			PartialFilter: bson.D{{Key: "archived", Value: false}},
		},
		{
			Fields:    []string{"title"},
			Name:      "title_ci",
			Collation: &options.Collation{Locale: "en", Strength: 2},
		},
	}
}

func TestIndexDefinitions_FromTagsAndIndexes(t *testing.T) {
	registerModel(t, &testRichIndexes{}, "test_rich_indexes", "")
	schema, _ := SchemaOf(&testRichIndexes{})

	byName := map[string]IndexDefinition{}
	for _, d := range schema.IndexDefinitions() {
		byName[d.Name] = d
	}

	cases := []struct {
		name string
		keys bson.D
		chk  func(IndexDefinition) bool
	}{
		{"tenant_hashed", bson.D{{Key: "tenant", Value: "hashed"}}, nil},
		{"slug_1", bson.D{{Key: "slug", Value: int32(1)}}, func(d IndexDefinition) bool { return d.Unique && d.Sparse }},
		{"title_text", bson.D{{Key: "title", Value: "text"}}, nil},
		{"expires_at_1", bson.D{{Key: "expires_at", Value: int32(1)}}, func(d IndexDefinition) bool { return d.ExpireAfterSeconds != nil && *d.ExpireAfterSeconds == 3600 }},
		{"score_-1", bson.D{{Key: "score", Value: int32(-1)}}, nil},
		{"address.city_1", bson.D{{Key: "address.city", Value: int32(1)}}, nil},
		{"tenant_1_created_at_-1", bson.D{{Key: "tenant", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}}, nil},
		{"slug_per_tenant", bson.D{{Key: "slug", Value: int32(1)}, {Key: "tenant", Value: int32(1)}}, func(d IndexDefinition) bool { return d.Unique && d.PartialFilter != nil }},
		{"title_ci", bson.D{{Key: "title", Value: int32(1)}}, func(d IndexDefinition) bool { return d.Collation != nil && d.Collation.Locale == "en" }},
	}
	for _, c := range cases {
		d, ok := byName[c.name]
		if !ok {
			t.Errorf("missing index definition %s (have %v)", c.name, keysOf(byName))
			continue
		}
		if !indexKeysEqual(d.Keys, c.keys) {
			t.Errorf("%s: keys %v, want %v", c.name, d.Keys, c.keys)
		}
		if c.chk != nil && !c.chk(d) {
			t.Errorf("%s: options not as expected: %+v", c.name, d)
		}
	}
	if len(byName) != len(cases) {
		t.Errorf("expected %d definitions, got %d: %v", len(cases), len(byName), keysOf(byName))
	}
}

func keysOf(m map[string]IndexDefinition) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEnforce_RichIndexes_CreatedAndStable(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testRichIndexes{}, "test_rich_indexes", "")

	if err := Enforce(ctx, db); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	coll := db.Collection("test_rich_indexes")
	existing, err := ListExistingIndexes(ctx, coll)
	if err != nil {
		t.Fatal(err)
	}

	if s, ok := existing["slug_1"]; !ok || !s.Unique || !s.Sparse {
		t.Errorf("slug_1: want unique sparse, got %+v", s)
	}
	if s, ok := existing["expires_at_1"]; !ok || !s.HasTTL || *s.ExpireAfterSeconds != 3600 {
		t.Errorf("expires_at_1: want ttl 3600, got %+v", s)
	}
	if s, ok := existing["title_text"]; !ok || !indexKeysEqual(s.Keys, bson.D{{Key: "title", Value: "text"}}) {
		t.Errorf("title_text: keys %v", s.Keys)
	}
	if s, ok := existing["tenant_hashed"]; !ok || !indexKeysEqual(s.Keys, bson.D{{Key: "tenant", Value: "hashed"}}) {
		t.Errorf("tenant_hashed: keys %v", s.Keys)
	}
	if s, ok := existing["address.city_1"]; !ok || s.Keys[0].Key != "address.city" {
		t.Errorf("address.city_1: %+v", s)
	}
	if s, ok := existing["slug_per_tenant"]; !ok || !s.Unique || !s.HasPartialFilter {
		t.Errorf("slug_per_tenant: want unique partial, got %+v", s)
	}
	if s, ok := existing["title_ci"]; !ok || !s.HasCollation {
		t.Errorf("title_ci: want collation, got %+v", s)
	}
	if s, ok := existing["tenant_1_created_at_-1"]; !ok || indexKeyString(s.Keys[1].Value) != "-1" {
		t.Errorf("tenant_1_created_at_-1: %+v", s)
	}

	// A second Enforce with the strict policy must see no mismatches.
	if err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchError}); err != nil {
		t.Fatalf("second Enforce reported a mismatch on indexes it just created: %v", err)
	}
}

type testTTLModel struct {
	Model     `bson:",inline"`
	ExpiresAt time.Time `bson:"expires_at" goodm:"ttl=7200"`
}

func TestEnforce_TTLMismatch_ErrorsThenRebuilds(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testTTLModel{}, "test_ttl", "")

	coll := db.Collection("test_ttl")
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(60),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = Enforce(ctx, db)
	var ee *EnforcementError
	if !errors.As(err, &ee) || !strings.Contains(ee.Message, "expires after 60s") {
		t.Fatalf("expected TTL mismatch error, got %v", err)
	}

	if err := Enforce(ctx, db, EnforceOptions{IndexMismatchPolicy: IndexMismatchRebuild}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	spec, _ := getIndexSpec(t, ctx, coll, "expires_at_1")
	if spec.ExpireAfterSeconds == nil || *spec.ExpireAfterSeconds != 7200 {
		t.Errorf("expected ttl 7200 after rebuild, got %+v", spec)
	}
}

type testSparseUnique struct {
	Model `bson:",inline"`
	Code  string `bson:"code" goodm:"unique,sparse"`
}

func TestEnforce_SparseUnique_AcceptedAndEnforced(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testSparseUnique{}, "test_sparse_unique", "")

	if err := Enforce(ctx, db); err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	// Two documents without the field must coexist under a sparse unique index.
	coll := db.Collection("test_sparse_unique")
	if _, err := coll.InsertMany(ctx, []interface{}{bson.D{{Key: "x", Value: 1}}, bson.D{{Key: "x", Value: 2}}}); err != nil {
		t.Fatalf("sparse unique index should allow documents missing the field: %v", err)
	}
	if err := Enforce(ctx, db); err != nil {
		t.Fatalf("second Enforce: %v", err)
	}
}

func TestMigrate_UsesDefinitions_NotIndexNames(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()
	registerModel(t, &testRichIndexes{}, "test_rich_indexes", "")

	schema, _ := SchemaOf(&testRichIndexes{})
	plan, err := PlanMigration(ctx, db, []*Schema{schema})
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, a := range plan.Actions {
		if a.Type == ActionCreateIndex {
			creates++
			if a.Definition == nil {
				t.Errorf("create action %s has no definition", a.IndexName)
			}
		}
	}
	if creates != len(schema.IndexDefinitions()) {
		t.Fatalf("expected %d create actions, got %d", len(schema.IndexDefinitions()), creates)
	}

	res, err := ExecuteMigration(ctx, db, plan, MigrateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("migration errors: %v", res.Errors)
	}

	existing, _ := ListExistingIndexes(ctx, db.Collection("test_rich_indexes"))
	s, ok := existing["tenant_1_created_at_-1"]
	if !ok || indexKeyString(s.Keys[1].Value) != "-1" {
		t.Errorf("descending compound key lost through migrate: %+v", s)
	}
	if s, ok := existing["slug_1"]; !ok || !s.Sparse || !s.Unique {
		t.Errorf("sparse unique option lost through migrate: %+v", s)
	}

	// Now everything matches: the plan must be empty apart from drift.
	plan, _ = PlanMigration(ctx, db, []*Schema{schema})
	for _, a := range plan.Actions {
		if a.Type != ActionFieldDrift {
			t.Errorf("unexpected action after migrate: %s", a.Description)
		}
	}
}

func TestMigrate_PlansRebuildForMismatchedIndex(t *testing.T) {
	ctx, db, cleanup := setupTestDB(t)
	defer cleanup()

	coll := db.Collection("test_users")
	createNonUniqueIndex(t, ctx, coll, "email")

	schema, _ := SchemaOf(&testUser{})
	plan, err := PlanMigration(ctx, db, []*Schema{schema})
	if err != nil {
		t.Fatal(err)
	}
	var rebuild *MigrationAction
	for i := range plan.Actions {
		if plan.Actions[i].Type == ActionRebuildIndex && plan.Actions[i].IndexName == "email_1" {
			rebuild = &plan.Actions[i]
		}
	}
	if rebuild == nil {
		t.Fatalf("expected a rebuild action for email_1, got %+v", plan.Actions)
	}

	res, err := ExecuteMigration(ctx, db, plan, MigrateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("migration errors: %v", res.Errors)
	}
	spec, _ := getIndexSpec(t, ctx, coll, "email_1")
	if !spec.Unique {
		t.Error("email_1 should be unique after migrate rebuild")
	}
}

func TestParseGoodmTag_IndexKindsAndTTL(t *testing.T) {
	fs := ParseGoodmTag("index=-1,sparse,ttl=90m,min=0.5,max=2.5")
	if !fs.Index || fs.IndexKind != "-1" || !fs.Sparse {
		t.Errorf("index/sparse: %+v", fs)
	}
	if fs.TTL == nil || *fs.TTL != 5400 {
		t.Errorf("ttl: %v", fs.TTL)
	}
	if fs.Min == nil || *fs.Min != 0.5 || fs.Max == nil || *fs.Max != 2.5 {
		t.Errorf("min/max: %v %v", fs.Min, fs.Max)
	}
	fs = ParseGoodmTag("ttl=0")
	if fs.TTL == nil || *fs.TTL != 0 {
		t.Errorf("ttl=0 should be an explicit zero, got %v", fs.TTL)
	}
}

func TestCompoundIndex_KeysAndNames(t *testing.T) {
	ci := NewUniqueCompoundIndex("tenant", "-created_at", "title:text")
	want := bson.D{{Key: "tenant", Value: int32(1)}, {Key: "created_at", Value: int32(-1)}, {Key: "title", Value: "text"}}
	if !indexKeysEqual(ci.Keys(), want) {
		t.Errorf("keys %v, want %v", ci.Keys(), want)
	}
	if n := compoundIndexName(ci); n != "tenant_1_created_at_-1_title_text" {
		t.Errorf("name %q", n)
	}
	ci.Name = "custom"
	if n := compoundIndexName(ci); n != "custom" {
		t.Errorf("custom name %q", n)
	}
	if s := TTL(90 * time.Second); *s != 90 {
		t.Errorf("TTL(90s) = %d", *s)
	}
}
