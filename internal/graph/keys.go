// Package graph porte l'extraction du graphe de connaissances, la
// résolution des entités et le chaînage temporel des relations. Il ne
// connaît ni SQL ni HTTP: il implémente le port memory.GraphExtractor et
// fournit à internal/store/postgres les clés déterministes que celui-ci
// écrit en base.
package graph

import (
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// entityNamespace est le namespace UUIDv5 des entités et des relations.
// C'est le namespace URL de la RFC 4122, le même que celui des unités de
// mémoire: les deux espaces de noms restent disjoints parce que les clés
// encodées n'ont pas la même forme.
var entityNamespace = uuid.NameSpaceURL

// CanonicalKey construit la clé canonique d'une entité résolue, sous la
// forme "type:nom-en-slug". C'est elle qui porte la déduplication des
// entités via l'index unique partiel graph_entities_canonical_idx.
//
// Le slug garde les lettres et les chiffres Unicode, donc "Café" devient
// "café" et non "caf". Translittérer les accents fusionnerait des entités
// distinctes dans un service dont la langue de travail est le français.
func CanonicalKey(entityType, displayName string) string {
	return strings.ToLower(strings.TrimSpace(entityType)) + ":" + Slug(displayName)
}

// UnresolvedKey construit la clé d'une entité que l'extracteur n'a pas su
// résoudre. Elle porte la conversation d'origine, ce qui garantit que deux
// "Paul" apparus dans deux conversations différentes restent deux lignes
// distinctes. Le service ne les fusionnera jamais de lui-même (règle 7.3
// de la spec).
func UnresolvedKey(entityType, displayName, conversationID string) string {
	return "unresolved-" + CanonicalKey(entityType, displayName) + "-" + conversationID
}

// EntityID dérive un identifiant stable d'une entité. Déterministe, donc un
// rejeu du même job d'extraction réécrit la même ligne au lieu d'en créer
// une seconde.
func EntityID(workspaceID, canonicalKey string) uuid.UUID {
	key := memory.LengthPrefixed(workspaceID) + memory.LengthPrefixed(canonicalKey)
	return uuid.NewSHA1(entityNamespace, []byte(key))
}

// DedupKey calcule la clé de déduplication d'une relation. La spec la
// décrit en section 4.8 avec un séparateur simple; on encode chaque
// composante par sa longueur pour la même raison qui a fait abandonner le
// séparateur simple sur UnitID: relation_type et target_literal sortent
// d'un modèle de langage et peuvent contenir n'importe quel caractère.
//
// validFrom est tronqué à la seconde et ramené en UTC. Ce n'est pas pour un
// recalcul depuis la base: la clé n'est calculée qu'ici, à l'extraction, et
// rien ne la reconstruit ensuite, ni la réévaluation ni la maintenance des
// chaînes. C'est pour que deux extractions du même fait retombent sur la
// même clé alors que la date qu'elles portent a transité par des chemins
// différents, un JSON, un timestamptz relu, un time.Now local, dont ni la
// précision ni le fuseau ne se ressemblent. Sans normalisation, le même
// fait produirait deux lignes.
func DedupKey(sourceEntityID uuid.UUID, relationType string,
	targetEntityID *uuid.UUID, targetLiteral string, validFrom *time.Time) string {

	target := "lit:" + strings.ToLower(strings.TrimSpace(targetLiteral))
	if targetEntityID != nil {
		target = "ent:" + targetEntityID.String()
	}

	// Un valid_from absent et un valid_from à l'époque zéro doivent donner
	// deux clés différentes: le premier veut dire "on ne sait pas depuis
	// quand", le second serait une date réelle.
	from := ""
	if validFrom != nil {
		from = "at:" + validFrom.UTC().Truncate(time.Second).Format(time.RFC3339)
	}

	return memory.LengthPrefixed(sourceEntityID.String()) +
		memory.LengthPrefixed(NormalizeRelationType(relationType)) +
		memory.LengthPrefixed(target) +
		memory.LengthPrefixed(from)
}

// RelationID dérive un identifiant stable d'une relation à partir de sa
// dedup_key. Même propriété que EntityID: un rejeu réécrit la même ligne.
func RelationID(workspaceID, dedupKey string) uuid.UUID {
	key := memory.LengthPrefixed(workspaceID) + memory.LengthPrefixed(dedupKey)
	return uuid.NewSHA1(entityNamespace, []byte(key))
}

// NormalizeRelationType ramène un prédicat à du snake_case minuscule. Le
// modèle produit tantôt "HAS_OBSERVED_STATE", tantôt "has observed state":
// sans normalisation, la liste single_valued_relations de la configuration
// ne reconnaîtrait qu'une forme sur deux.
func NormalizeRelationType(s string) string {
	return joinRunes(strings.ToLower(strings.TrimSpace(s)), '_')
}

// Slug ramène un nom d'affichage à une forme comparable: minuscules,
// lettres et chiffres Unicode conservés, tout le reste réduit à un tiret
// unique, sans tiret en tête ni en queue.
//
// Exportée parce que la recherche par graphe doit comparer la partie
// locale d'une clé d'identité ("user:paul") à la partie nominale d'une clé
// canonique ("person:paul"), et qu'une seconde implémentation du même
// découpage finirait par diverger de celle qui a produit les clés.
func Slug(s string) string {
	return joinRunes(strings.ToLower(strings.TrimSpace(s)), '-')
}

// joinRunes conserve les lettres, les chiffres et les marques diacritiques
// combinantes, et remplace toute autre suite de caractères par un unique
// séparateur, sans séparateur en tête ni en queue.
//
// Les marques combinantes (catégorie Mn) comptent parce qu'un "é" a deux
// écritures Unicode: un seul point de code en NFC, ou "e" suivi d'un accent
// aigu combinant en NFD. Sans ce cas, la forme décomposée perdait son accent
// et "café" devenait "cafe", donc une entité différente de sa jumelle NFC,
// silencieusement.
//
// Ça ne réconcilie pas les deux formes pour autant: la bibliothèque standard
// n'a pas de normalisation Unicode, et l'ajouter voudrait dire une nouvelle
// dépendance que les contraintes du projet interdisent. Deux normalisations
// du même nom restent donc deux entités, que le service ne fusionnera jamais
// de lui-même, exactement comme deux "Paul" non résolus. Voir les
// limitations de la spec.
func joinRunes(s string, sep rune) string {
	var b strings.Builder
	pending := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) {
			if pending && b.Len() > 0 {
				b.WriteRune(sep)
			}
			pending = false
			b.WriteRune(r)
			continue
		}
		pending = true
	}
	return b.String()
}
