package memory

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestMatchIdentity(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		key      string
		want     bool
	}{
		{"exact", []string{"agent:cuisine"}, "agent:cuisine", true},
		{"exact refuse autre", []string{"agent:cuisine"}, "agent:jardinage", false},
		{"prefixe", []string{"agent:*"}, "agent:jardinage", true},
		{"prefixe ne traverse pas le type", []string{"agent:*"}, "user:paul", false},
		{"joker total", []string{"*"}, "user:paul", true},
		{"liste vide refuse", nil, "user:paul", false},
		{"une entree parmi plusieurs", []string{"user:alice", "agent:*"}, "agent:x", true},
		// Une etoile au milieu n'est pas un motif: seul le suffixe compte.
		{"etoile au milieu est litterale", []string{"ag*nt:x"}, "agent:x", false},
		// Ce que tente en premier un attaquant qui cherche a usurper une
		// identite: une etoile en tete n'est pas un motif de suffixe reconnu,
		// elle est donc comparee litteralement et ne matche jamais.
		{"etoile en tete est litterale", []string{"*:paul"}, "user:paul", false},
		// Une etoile doublee ne se reduit pas au joker total: apres avoir
		// retire le seul "*" final, il en reste un, litteral.
		{"etoile doublee n'est pas le joker total", []string{"**"}, "user:paul", false},
		{"motif vide refuse une cle non vide", []string{""}, "user:paul", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchIdentity(c.patterns, c.key); got != c.want {
				t.Errorf("MatchIdentity(%v, %q) = %v, want %v",
					c.patterns, c.key, got, c.want)
			}
		})
	}
}

func TestPrincipalAuthorize(t *testing.T) {
	p := &Principal{
		ClientID:          uuid.New(),
		Label:             "orchestrateur",
		WorkspaceID:       "ws1",
		AllowedIdentities: []string{"agent:*", "user:paul"},
	}

	if err := p.Authorize("ws1", "agent:cuisine"); err != nil {
		t.Errorf("cas nominal refusé: %v", err)
	}
	// Le coeur du critere 4 cote authentification: on ne lit pas le workspace
	// d'un autre projet en changeant un champ JSON.
	if err := p.Authorize("ws2", "agent:cuisine"); !errors.Is(err, ErrForbidden) {
		t.Errorf("un autre workspace doit être refusé, got %v", err)
	}
	if err := p.Authorize("ws1", "user:alice"); !errors.Is(err, ErrForbidden) {
		t.Errorf("une identité hors motif doit être refusée, got %v", err)
	}
	if err := p.Authorize("", "agent:cuisine"); !errors.Is(err, ErrForbidden) {
		t.Errorf("un workspace vide doit être refusé, got %v", err)
	}
	if err := p.Authorize("ws1", ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("une identité vide doit être refusée, got %v", err)
	}
}
