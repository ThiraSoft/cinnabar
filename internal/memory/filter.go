package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MetadataFilter est une condition sur les metadata d'un message: une
// feuille (Key, Op, Value) ou un groupe (All, Any), jamais les deux. Le
// service ne donne de sens à aucune clé, il ne fait que comparer: c'est ce
// qui le garde généraliste.
//
// Le filtre ne peut que retirer des résultats. Il s'ajoute à la règle
// d'accès dans le SQL, il ne la remplace jamais, et il est évalué par la
// fonction cinnabar_metadata_match (migration 006), qui suppose un filtre
// déjà passé par Validate.
type MetadataFilter struct {
	Key   string           `json:"key,omitempty"`
	Op    string           `json:"op,omitempty"`
	Value json.RawMessage  `json:"value,omitempty"`
	All   []MetadataFilter `json:"all,omitempty"`
	Any   []MetadataFilter `json:"any,omitempty"`
}

// Bornes du filtre. Il part en paramètre d'une fonction évaluée sur chaque
// ligne candidate: sans borne, un client pourrait faire payer à tout le
// service un filtre de dix mille feuilles.
const (
	MaxFilterDepth    = 4
	MaxFilterLeaves   = 32
	MaxFilterInValues = 256
	MaxFilterKeyBytes = 128
)

// ErrInvalidFilter accompagne tout refus de Validate. C'est une faute de
// l'appelant, que la couche HTTP traduit en 400.
var ErrInvalidFilter = errors.New("invalid metadata filter")

var filterOps = map[string]bool{
	"eq": true, "ne": true, "lt": true, "lte": true,
	"gt": true, "gte": true, "in": true, "exists": true,
}

// Validate vérifie la grammaire et les bornes. Un filtre nil est valide et
// ne filtre rien.
func (f *MetadataFilter) Validate() error {
	if f == nil {
		return nil
	}
	leaves := 0
	return f.validate(1, &leaves)
}

func (f *MetadataFilter) validate(depth int, leaves *int) error {
	if depth > MaxFilterDepth {
		return fmt.Errorf("%w: deeper than %d", ErrInvalidFilter, MaxFilterDepth)
	}
	isLeaf := f.Key != "" || f.Op != "" || f.Value != nil
	switch {
	case isLeaf && (f.All != nil || f.Any != nil):
		return fmt.Errorf("%w: a node is either a leaf or a group", ErrInvalidFilter)
	case f.All != nil && f.Any != nil:
		return fmt.Errorf("%w: all and any in the same node", ErrInvalidFilter)
	case f.All != nil || f.Any != nil:
		children := f.All
		if f.Any != nil {
			children = f.Any
		}
		if len(children) == 0 {
			return fmt.Errorf("%w: empty group", ErrInvalidFilter)
		}
		for i := range children {
			if err := children[i].validate(depth+1, leaves); err != nil {
				return err
			}
		}
		return nil
	case !isLeaf:
		return fmt.Errorf("%w: empty node", ErrInvalidFilter)
	}

	*leaves++
	if *leaves > MaxFilterLeaves {
		return fmt.Errorf("%w: more than %d leaves", ErrInvalidFilter, MaxFilterLeaves)
	}
	if f.Key == "" || len(f.Key) > MaxFilterKeyBytes {
		return fmt.Errorf("%w: key must be 1 to %d bytes", ErrInvalidFilter, MaxFilterKeyBytes)
	}
	if !filterOps[f.Op] {
		return fmt.Errorf("%w: unknown op %q", ErrInvalidFilter, f.Op)
	}
	if f.Value == nil {
		return fmt.Errorf("%w: value is required", ErrInvalidFilter)
	}

	switch f.Op {
	case "exists":
		if k := jsonKind(f.Value); k != 't' && k != 'f' {
			return fmt.Errorf("%w: exists takes a boolean", ErrInvalidFilter)
		}
	case "in":
		var vals []json.RawMessage
		if jsonKind(f.Value) != '[' || json.Unmarshal(f.Value, &vals) != nil {
			return fmt.Errorf("%w: in takes an array", ErrInvalidFilter)
		}
		if len(vals) == 0 || len(vals) > MaxFilterInValues {
			return fmt.Errorf("%w: in takes 1 to %d values", ErrInvalidFilter, MaxFilterInValues)
		}
	case "lt", "lte", "gt", "gte":
		if k := jsonKind(f.Value); k != '"' && k != '0' {
			return fmt.Errorf("%w: %s takes a number or a string", ErrInvalidFilter, f.Op)
		}
	}
	return nil
}

// jsonKind rend le premier octet significatif d'une valeur JSON, ramené à
// '0' pour un nombre: assez pour distinguer les types sans la décoder.
func jsonKind(raw json.RawMessage) byte {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return 0
	}
	if b[0] == '-' || (b[0] >= '0' && b[0] <= '9') {
		return '0'
	}
	return b[0]
}
