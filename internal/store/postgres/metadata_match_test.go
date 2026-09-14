package postgres

import (
	"context"
	"testing"
)

func TestMetadataMatchFunction(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	meta := `{"belief":"oui","importance":5,"day":"2026-09-14","lore_id":"nine-1","flag":true,"none":null}`
	for _, c := range []struct {
		filter string
		want   bool
	}{
		{`{"key":"belief","op":"eq","value":"oui"}`, true},
		{`{"key":"belief","op":"eq","value":"non"}`, false},
		{`{"key":"belief","op":"ne","value":"non"}`, true},
		{`{"key":"absent","op":"ne","value":"non"}`, true},
		{`{"key":"absent","op":"eq","value":"non"}`, false},
		{`{"key":"importance","op":"eq","value":5.0}`, true},
		{`{"key":"importance","op":"gte","value":5}`, true},
		{`{"key":"importance","op":"gt","value":5}`, false},
		{`{"key":"importance","op":"lt","value":10}`, true},
		{`{"key":"importance","op":"lte","value":4}`, false},
		{`{"key":"importance","op":"lt","value":"9"}`, false},
		{`{"key":"day","op":"lt","value":"2026-10-01"}`, true},
		{`{"key":"day","op":"gte","value":"2026-10-01"}`, false},
		{`{"key":"absent","op":"lt","value":10}`, false},
		{`{"key":"lore_id","op":"in","value":["nine-4","nine-1"]}`, true},
		{`{"key":"lore_id","op":"in","value":["nine-4"]}`, false},
		{`{"key":"flag","op":"exists","value":true}`, true},
		{`{"key":"absent","op":"exists","value":true}`, false},
		{`{"key":"absent","op":"exists","value":false}`, true},
		{`{"key":"none","op":"eq","value":null}`, true},
		{`{"all":[{"key":"belief","op":"eq","value":"oui"},{"key":"importance","op":"gt","value":3}]}`, true},
		{`{"all":[{"key":"belief","op":"eq","value":"oui"},{"key":"importance","op":"gt","value":8}]}`, false},
		{`{"any":[{"key":"importance","op":"gt","value":8},{"key":"lore_id","op":"in","value":["nine-1"]}]}`, true},
		{`{"any":[{"key":"importance","op":"gt","value":8},{"key":"absent","op":"exists","value":true}]}`, false},
	} {
		var got bool
		if err := pool.QueryRow(ctx,
			`SELECT cinnabar_metadata_match($1::jsonb, $2::jsonb)`, meta, c.filter).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.filter, err)
		}
		if got != c.want {
			t.Errorf("%s = %v, want %v", c.filter, got, c.want)
		}
	}

	var got bool
	if err := pool.QueryRow(ctx,
		`SELECT cinnabar_metadata_match($1::jsonb, NULL)`, meta).Scan(&got); err != nil || !got {
		t.Errorf("un filtre NULL doit tout laisser passer: %v, %v", got, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT cinnabar_metadata_match('{}'::jsonb, '{"key":"a","op":"ne","value":1}')`).Scan(&got); err != nil || !got {
		t.Errorf("ne sur des metadata vides: %v, %v", got, err)
	}
}
