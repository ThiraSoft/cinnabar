package memory

import (
	"errors"
	"fmt"
	"strings"
)

// ErrForbidden couvre les refus d'autorisation. Les handlers HTTP le
// traduisent en 403 sans exposer le détail à l'appelant.
var ErrForbidden = errors.New("forbidden")

// MatchIdentity teste une clé d'identité contre une liste de motifs. Un motif
// est soit une identité exacte, soit un préfixe terminé par une étoile, soit
// l'étoile seule. Une étoile ailleurs qu'en fin de motif est littérale, ce qui
// évite d'écrire un moteur de glob pour un besoin qui ne le demande pas.
func MatchIdentity(patterns []string, key string) bool {
	if key == "" {
		return false
	}
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(key, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if p == key {
			return true
		}
	}
	return false
}

// AuthorizeWorkspace vérifie seulement l'appartenance au workspace, sans
// identité à faire correspondre. Exportée pour qu'une route qui n'a pas de
// champ d'identité unique à vérifier (POST /v1/conversations, dont le seul
// équivalent est une liste de participants vérifiée à part) applique
// exactement la même règle que Authorize plutôt qu'une comparaison
// réécrite à la main: une comparaison manuelle (workspaceID != p.WorkspaceID)
// laisserait passer un workspace_id vide comparé à un principal dont le
// WorkspaceID serait lui aussi vide, un cas qu'Authorize refuse
// explicitement.
func (p *Principal) AuthorizeWorkspace(workspaceID string) error {
	if workspaceID == "" {
		return fmt.Errorf("%w: empty workspace_id", ErrForbidden)
	}
	if workspaceID != p.WorkspaceID {
		return fmt.Errorf("%w: workspace mismatch", ErrForbidden)
	}
	return nil
}

// Authorize vérifie que ce principal peut agir dans ce workspace sous cette
// identité. Les deux vérifications sont indépendantes: la première isole les
// workspaces, la seconde empêche un client d'usurper une identité.
func (p *Principal) Authorize(workspaceID, identity string) error {
	if err := p.AuthorizeWorkspace(workspaceID); err != nil {
		return err
	}
	if !MatchIdentity(p.AllowedIdentities, identity) {
		return fmt.Errorf("%w: identity not allowed", ErrForbidden)
	}
	return nil
}
