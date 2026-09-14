package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Le jeu de données d'évaluation est écrit à la main, et pour partie généré,
// donc il dérive. Ces vérifications sont structurelles et tournent dans la
// suite: un corpus incohérent produit des chiffres qui semblent valides, ce
// qui est la pire des sorties. Elles ne mesurent rien du service, elles
// mesurent l'instrument.
func loadDataset(t *testing.T) dataset {
	t.Helper()
	raw, err := os.ReadFile("dataset.json")
	if err != nil {
		t.Fatalf("lecture du jeu de données: %v", err)
	}
	var ds dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatalf("décodage du jeu de données: %v", err)
	}
	if len(ds.Conversations) == 0 || len(ds.Queries) == 0 {
		t.Fatal("jeu de données vide: les vérifications ne prouveraient rien")
	}
	return ds
}

// TestDatasetIdentifiantsUniques: deux messages qui partagent un id rendent
// toute requête qui le cible ambiguë, et le rappel mesuré devient faux sans
// qu'aucune erreur ne se déclenche.
func TestDatasetIdentifiantsUniques(t *testing.T) {
	ds := loadDataset(t)
	vu := map[string]string{}
	for _, c := range ds.Conversations {
		for _, m := range c.Messages {
			if m.ID == "" {
				continue
			}
			if autre, deja := vu[m.ID]; deja {
				t.Errorf("id %q présent dans %s et dans %s", m.ID, autre, c.ConversationID)
			}
			vu[m.ID] = c.ConversationID
		}
	}
}

// TestDatasetAttendusExistent: un expected_ids qui pointe un id absent fait
// échouer la requête pour une raison qui n'a rien à voir avec le service.
func TestDatasetAttendusExistent(t *testing.T) {
	ds := loadDataset(t)
	connus := map[string]bool{}
	for _, c := range ds.Conversations {
		for _, m := range c.Messages {
			if m.ID != "" {
				connus[m.ID] = true
			}
		}
	}
	for _, q := range ds.Queries {
		for _, want := range q.ExpectedIDs {
			if !connus[want] {
				t.Errorf("requête %s attend l'id %q, qui n'existe dans aucun message",
					q.Name, want)
			}
		}
	}
}

// TestDatasetFamillesNegativesSansAttendu: une requête negative ou acl réussit
// en ne rendant rien. Lui donner un expected_ids non vide est contradictoire,
// et le harnais l'évaluerait alors comme une requête positive.
func TestDatasetFamillesNegativesSansAttendu(t *testing.T) {
	ds := loadDataset(t)
	for _, q := range ds.Queries {
		switch q.Family {
		case "negative", "acl":
			if len(q.ExpectedIDs) != 0 {
				t.Errorf("requête %s de famille %s a un expected_ids non vide: %v",
					q.Name, q.Family, q.ExpectedIDs)
			}
		default:
			if len(q.ExpectedIDs) == 0 {
				t.Errorf("requête %s de famille %s n'attend aucun id: elle ne peut "+
					"pas réussir", q.Name, q.Family)
			}
		}
	}
}

// TestDatasetRolesCoherents: le service refuse un rôle inconnu, et un
// user:* qui parle en assistant fausserait la construction des unités
// d'indexation, dont le préambule cite les rôles.
func TestDatasetRolesCoherents(t *testing.T) {
	ds := loadDataset(t)
	for _, c := range ds.Conversations {
		for _, m := range c.Messages {
			switch {
			case strings.HasPrefix(m.AuthorKey, "user:"):
				if m.Role != "user" {
					t.Errorf("%s: %s parle avec le rôle %q", c.ConversationID, m.AuthorKey, m.Role)
				}
			case strings.HasPrefix(m.AuthorKey, "agent:"):
				if m.Role != "assistant" {
					t.Errorf("%s: %s parle avec le rôle %q", c.ConversationID, m.AuthorKey, m.Role)
				}
			default:
				t.Errorf("%s: identité %q hors convention user:/agent:",
					c.ConversationID, m.AuthorKey)
			}
			if strings.TrimSpace(m.Content) == "" {
				t.Errorf("%s: message vide", c.ConversationID)
			}
		}
	}
}

// TestDatasetAuteursSontParticipants: le service accumule les participants à
// l'écriture, donc un auteur absent de la liste déclarée finit participant
// quand même. Le corpus doit dire la vérité sur qui parle, sinon une requête
// d'ACL peut réussir ou échouer pour la mauvaise raison.
func TestDatasetAuteursSontParticipants(t *testing.T) {
	ds := loadDataset(t)
	for _, c := range ds.Conversations {
		declares := map[string]bool{}
		for _, p := range c.Participants {
			declares[p] = true
		}
		for _, m := range c.Messages {
			if !declares[m.AuthorKey] {
				t.Errorf("%s: %s parle sans être déclaré participant (%v)",
					c.ConversationID, m.AuthorKey, c.Participants)
			}
		}
	}
}

// TestDatasetDatesCroissantes: le chaînage temporel ne ferme une observation
// que sur une observation strictement postérieure. Une date qui recule dans
// une conversation rend donc une requête temporelle inmesurable.
func TestDatasetDatesCroissantes(t *testing.T) {
	ds := loadDataset(t)
	for _, c := range ds.Conversations {
		var prec time.Time
		for i, m := range c.Messages {
			if m.CreatedAt == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339, m.CreatedAt)
			if err != nil {
				t.Errorf("%s message %d: created_at %q illisible: %v",
					c.ConversationID, i, m.CreatedAt, err)
				continue
			}
			if !prec.IsZero() && at.Before(prec) {
				t.Errorf("%s message %d: %s recule par rapport à %s",
					c.ConversationID, i, at.Format(time.RFC3339), prec.Format(time.RFC3339))
			}
			prec = at
		}
	}
}

// TestDatasetScopesValides: un scope inconnu fait échouer la déclaration de
// conversation à l'ingestion, donc l'évaluation entière.
func TestDatasetScopesValides(t *testing.T) {
	ds := loadDataset(t)
	valides := map[string]bool{
		"": true, "private": true, "participants": true,
		"workspace": true, "explicit": true,
	}
	for _, c := range ds.Conversations {
		if !valides[c.Scope] {
			t.Errorf("%s: scope %q inconnu", c.ConversationID, c.Scope)
		}
	}
}

// TestDatasetRequetesACLInatteignables est la vérification qui compte le plus,
// parce qu'une requête d'ACL fausse est pire qu'absente: elle donne
// l'illusion que la frontière de lecture est mesurée. Le demandeur d'une
// requête acl ne doit être participant d'aucune conversation en scope private.
func TestDatasetRequetesACLInatteignables(t *testing.T) {
	ds := loadDataset(t)
	for _, q := range ds.Queries {
		if q.Family != "acl" {
			continue
		}
		for _, c := range ds.Conversations {
			if c.Scope != "private" {
				continue
			}
			for _, p := range c.Participants {
				if p == q.RequesterKey {
					t.Errorf("requête acl %s: son demandeur %s participe à la "+
						"conversation privée %s, donc la requête ne mesure pas "+
						"ce qu'elle annonce", q.Name, q.RequesterKey, c.ConversationID)
				}
			}
			for _, m := range c.Messages {
				if m.AuthorKey == q.RequesterKey {
					t.Errorf("requête acl %s: son demandeur %s parle dans la "+
						"conversation privée %s", q.Name, q.RequesterKey, c.ConversationID)
				}
			}
		}
	}
}
