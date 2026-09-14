package memory

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func parseFilter(t *testing.T, raw string) *MetadataFilter {
	t.Helper()
	var f MetadataFilter
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("json %s: %v", raw, err)
	}
	return &f
}

func TestMetadataFilterValidateAccepts(t *testing.T) {
	for _, raw := range []string{
		`{"key":"belief","op":"eq","value":"oui"}`,
		`{"key":"belief","op":"ne","value":null}`,
		`{"key":"importance","op":"gte","value":5}`,
		`{"key":"day","op":"lt","value":"2026-09-14"}`,
		`{"key":"lore_id","op":"in","value":["a","b"]}`,
		`{"key":"about","op":"exists","value":false}`,
		`{"all":[{"key":"a","op":"eq","value":1},{"any":[{"key":"b","op":"exists","value":true}]}]}`,
	} {
		if err := parseFilter(t, raw).Validate(); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestMetadataFilterValidateRejects(t *testing.T) {
	deep := `{"key":"a","op":"eq","value":1}`
	for i := 0; i < MaxFilterDepth; i++ {
		deep = `{"all":[` + deep + `]}`
	}
	many := `{"any":[` + strings.Repeat(`{"key":"a","op":"eq","value":1},`, MaxFilterLeaves) +
		`{"key":"a","op":"eq","value":1}]}`
	bigIn := `{"key":"a","op":"in","value":[` + strings.Repeat(`1,`, MaxFilterInValues) + `1]}`

	for name, raw := range map[string]string{
		"vide":                 `{}`,
		"sans op":              `{"key":"a","value":1}`,
		"op inconnu":           `{"key":"a","op":"like","value":"x"}`,
		"sans clé":             `{"op":"eq","value":1}`,
		"clé trop longue":      `{"key":"` + strings.Repeat("k", MaxFilterKeyBytes+1) + `","op":"eq","value":1}`,
		"sans valeur":          `{"key":"a","op":"eq"}`,
		"feuille et groupe":    `{"key":"a","op":"eq","value":1,"all":[{"key":"b","op":"eq","value":1}]}`,
		"all et any":           `{"all":[{"key":"a","op":"eq","value":1}],"any":[{"key":"b","op":"eq","value":1}]}`,
		"groupe vide":          `{"all":[]}`,
		"in sans tableau":      `{"key":"a","op":"in","value":"x"}`,
		"in vide":              `{"key":"a","op":"in","value":[]}`,
		"exists non booléen":   `{"key":"a","op":"exists","value":1}`,
		"ordre sur un booléen": `{"key":"a","op":"lt","value":true}`,
		"ordre sur un objet":   `{"key":"a","op":"gte","value":{"x":1}}`,
		"trop profond":         deep,
		"trop de feuilles":     many,
		"in trop long":         bigIn,
	} {
		err := parseFilter(t, raw).Validate()
		if !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("%s: err = %v, want ErrInvalidFilter", name, err)
		}
	}
}

func TestMetadataFilterNilIsValid(t *testing.T) {
	var f *MetadataFilter
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}
