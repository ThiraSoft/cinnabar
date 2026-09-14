package postgres

import (
	"encoding/json"
	"fmt"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// restriction met en paramètres SQL les deux filtres facultatifs d'une
// CandidateQuery. Un nil, pour l'un comme pour l'autre, veut dire sans
// filtre: chaque stratégie le teste avec IS NULL avant de filtrer.
//
// Le filtre est revalidé ici même si la couche HTTP l'a déjà fait:
// cinnabar_metadata_match suppose une grammaire valide, et ce paquet ne
// laisse pas cette précondition à la charge de ses appelants.
func restriction(q memory.CandidateQuery) (convs []string, filter *string, err error) {
	if len(q.ConversationIDs) > 0 {
		convs = q.ConversationIDs
	}
	if q.MetadataFilter != nil {
		if err := q.MetadataFilter.Validate(); err != nil {
			return nil, nil, err
		}
		b, err := json.Marshal(q.MetadataFilter)
		if err != nil {
			return nil, nil, fmt.Errorf("encode metadata filter: %w", err)
		}
		s := string(b)
		filter = &s
	}
	return convs, filter, nil
}
