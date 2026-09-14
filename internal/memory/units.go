package memory

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// unitNamespace est le namespace URL de la RFC 4122. Il est figé: le changer
// invaliderait tous les identifiants déjà calculés.
var unitNamespace = uuid.NameSpaceURL

// ShouldIndex dit si un rôle produit une unité vectorielle.
func ShouldIndex(cfg config.Indexing, role string) bool {
	switch role {
	case "user":
		return cfg.IndexUserMessages
	case "assistant":
		return cfg.IndexAgentMessages
	case "system":
		return cfg.IndexSystemMessages
	case "tool":
		return cfg.IndexToolMessages
	default:
		return false
	}
}

// UnitID calcule l'identifiant déterministe d'une unité. C'est le second
// verrou d'idempotence: rejouer un événement retombe sur la même ligne, la
// clé d'idempotence du message étant le premier.
//
// Format de la clé figé: chaque composante est préfixée par sa longueur en
// octets ("longueur:contenu"), ce qui rend la concaténation des quatre
// composantes non ambiguë quel que soit leur contenu, y compris un ":" ou un
// autre caractère de séparation habituel à l'intérieur d'une valeur. Changer
// ce format déplacerait tous les identifiants déjà calculés vers de
// nouvelles valeurs et orphelinerait donc toutes les unités déjà indexées.
func UnitID(anchorID uuid.UUID, model, strategy string, version int) uuid.UUID {
	key := LengthPrefixed(anchorID.String()) +
		LengthPrefixed(model) +
		LengthPrefixed(strategy) +
		LengthPrefixed(strconv.Itoa(version))
	return uuid.NewSHA1(unitNamespace, []byte(key))
}

// LengthPrefixed encode une composante en "longueur:contenu". Le décodage
// n'a jamais besoin de chercher un séparateur à l'intérieur du contenu: il
// lit la longueur, saute le ":", puis consomme exactement ce nombre
// d'octets. Deux composantes différentes ne peuvent donc jamais produire la
// même concaténation, contrairement à un simple séparateur comme "|".
//
// Exportée parce que la dedup_key des relations du graphe a exactement le
// même besoin, sur des composantes encore moins contrôlées puisqu'elles
// sortent d'un modèle de langage. Une deuxième implémentation du même
// encodage divergerait un jour.
func LengthPrefixed(s string) string {
	return strconv.Itoa(len(s)) + ":" + s
}

// contextHeader et contextFooter encadrent le bloc de contexte quand il n'est
// pas vide. Ils comptent dans le budget de caractères comme le reste.
const (
	contextHeader = "Contexte précédent :\n"
	contextFooter = "\n"
)

// BuildUnit assemble le texte d'indexation d'un message contextualisé. Le
// booléen est faux quand le rôle du message n'est pas indexé.
//
// previous est ordonné par sequence_number croissant et peut contenir des
// messages supprimés, qui sont écartés ici.
func BuildUnit(cfg config.Indexing, model, scope string, participants []string,
	anchor Message, previous []Message) (Unit, bool) {

	if !ShouldIndex(cfg, anchor.Role) {
		return Unit{}, false
	}

	kept := make([]Message, 0, len(previous))
	for _, m := range previous {
		if m.DeletedAt != nil {
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) > cfg.PreviousMessages {
		kept = kept[len(kept)-cfg.PreviousMessages:]
	}

	preamble := ""
	if len(participants) > 0 {
		preamble = "Conversation impliquant " + joinFrench(participants) + ".\n\n"
	}
	main := fmt.Sprintf("Message principal :\n%s : %s", anchor.AuthorKey, anchor.Content)

	// Budget : le préambule et le message principal sont prioritaires, le
	// contexte est sacrifié en premier. On construit le bloc de contexte en
	// partant du message le plus récent et en remontant, en s'arrêtant au
	// premier qui ne tient plus dans ce qu'il reste de budget. La liste des
	// messages réellement inclus tombe directement de cette marche: pas
	// besoin de deviner ensuite, en cherchant du texte dans une chaîne déjà
	// tronquée, quel message a survécu.
	contextBudget := cfg.MaxChars - len(preamble) - len(main)
	var included []Message
	if contextBudget > 0 {
		used := len(contextHeader) + len(contextFooter)
		for i := len(kept) - 1; i >= 0; i-- {
			line := kept[i].AuthorKey + " : " + kept[i].Content + "\n"
			if used+len(line) > contextBudget {
				break
			}
			used += len(line)
			included = append(included, kept[i])
		}
		reverse(included) // on avait accumulé du plus récent au plus ancien
	}

	var ctxBlock strings.Builder
	if len(included) > 0 {
		ctxBlock.WriteString(contextHeader)
		for _, m := range included {
			fmt.Fprintf(&ctxBlock, "%s : %s\n", m.AuthorKey, m.Content)
		}
		ctxBlock.WriteString(contextFooter)
	}

	text := preamble + ctxBlock.String() + main
	if len(text) > cfg.MaxChars {
		// Même sans contexte, le préambule et le message principal dépassent
		// la limite: dernier recours, on tronque le message principal en
		// gardant le préambule intact.
		room := cfg.MaxChars - len(preamble)
		if room < 0 {
			room = 0
		}
		text = preamble + TruncateRunes(main, room)
		included = nil
	}
	if len(text) > cfg.MaxChars {
		// Dernier filet de sécurité: même le préambule seul dépasse la
		// limite (liste de participants démesurée). La limite annoncée par
		// MaxChars ne doit jamais être franchie, quitte à couper dans le
		// préambule ici.
		text = TruncateRunes(text, cfg.MaxChars)
	}

	start := anchor.SequenceNumber
	if len(included) > 0 {
		start = included[0].SequenceNumber
	}

	return Unit{
		MemoryUnitID:    UnitID(anchor.MessageID, model, cfg.Strategy, cfg.Version),
		WorkspaceID:     anchor.WorkspaceID,
		ConversationID:  anchor.ConversationID,
		AnchorMessageID: anchor.MessageID,
		StartSequence:   start,
		EndSequence:     anchor.SequenceNumber,
		EmbeddingText:   text,
		EmbeddingModel:  model,
		Strategy:        cfg.Strategy,
		Version:         cfg.Version,
		Scope:           scope,
	}, true
}

// reverse inverse l'ordre en place.
func reverse(msgs []Message) {
	for l, r := 0, len(msgs)-1; l < r; l, r = l+1, r-1 {
		msgs[l], msgs[r] = msgs[r], msgs[l]
	}
}

// TruncateRunes coupe sur une frontière de rune, pour ne pas produire d'UTF-8
// invalide au milieu d'un caractère accentué. Une rune multi-octets qui
// tient entièrement dans max est conservée: seule une rune réellement
// coupée en deux par la limite est retirée.
//
// Exportée parce que le stockage en a besoin aussi (JobRepo.Fail borne
// last_error): Postgres refuse une chaîne qui n'est pas de l'UTF-8 valide,
// donc une coupe à l'octet y transforme un job en échec en un job bloqué en
// running dont la vraie cause est perdue. Une seule implémentation pour les
// deux, plutôt qu'une copie qui divergerait.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	b := s[:max]
	for len(b) > 0 {
		r, size := utf8.DecodeLastRuneInString(b)
		if r != utf8.RuneError || size != 1 {
			// La dernière rune de b est complète et valide: elle tenait
			// dans max, on la garde.
			break
		}
		// b se termine par une rune coupée en cours d'encodage (ou par un
		// octet invalide): on le retire et on retente.
		b = b[:len(b)-1]
	}
	return b
}

func joinFrench(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " et " + items[len(items)-1]
	}
}
