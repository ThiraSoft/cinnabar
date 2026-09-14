package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/llm"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// ChatClient est le sous-ensemble de llm.Client dont l'extracteur a
// besoin. Le déclarer ici plutôt que d'importer le type concret rend
// l'extracteur testable sans réseau, ce qui est la seule façon de tester
// le parsing d'une réponse malformée.
type ChatClient interface {
	Chat(ctx context.Context, r llm.ChatRequest) (string, error)
}

// Extractor implémente memory.GraphExtractor par un appel à un modèle de
// langage compatible OpenAI, contraint par un json_schema.
type Extractor struct {
	chat ChatClient
	cfg  config.Graph
}

// NewExtractor construit un extracteur. cfg n'est utilisée pour l'instant
// que par les appelants (context_messages notamment est appliqué en amont,
// dans le worker qui construit GraphExtractorInput); elle est conservée
// ici parce que la signature du port l'attend et qu'un futur réglage
// (max_hops, par exemple) pourrait s'y ajouter sans casser l'appelant.
func NewExtractor(chat ChatClient, cfg config.Graph) *Extractor {
	return &Extractor{chat: chat, cfg: cfg}
}

// rawEntity et rawRelation sont la forme brute rendue par le modèle, avant
// résolution des temp_id. Elles ne sortent jamais de ce fichier.
type rawEntity struct {
	TempID       string   `json:"temp_id"`
	EntityType   string   `json:"entity_type"`
	DisplayName  string   `json:"display_name"`
	CanonicalKey *string  `json:"canonical_key"`
	Aliases      []string `json:"aliases"`
	Resolved     bool     `json:"resolved"`
}

type rawRelation struct {
	SourceTempID     string   `json:"source_temp_id"`
	RelationType     string   `json:"relation_type"`
	TargetTempID     *string  `json:"target_temp_id"`
	TargetLiteral    *string  `json:"target_literal"`
	ObservedAt       *string  `json:"observed_at"`
	ValidFrom        *string  `json:"valid_from"`
	ValidUntil       *string  `json:"valid_until"`
	Confidence       float64  `json:"confidence"`
	SourceMessageIDs []string `json:"source_message_ids"`
}

type rawExtraction struct {
	Entities  []rawEntity   `json:"entities"`
	Relations []rawRelation `json:"relations"`
}

// Extract appelle le modèle et convertit sa réponse en extraction prête à
// écrire. Une réponse illisible fait échouer le job, qui repart en
// backoff; une réponse partiellement absurde est rabotée par Validate.
func (e *Extractor) Extract(ctx context.Context,
	in memory.GraphExtractorInput) (memory.GraphExtraction, error) {

	reply, err := e.chat.Chat(ctx, llm.ChatRequest{
		System:      systemPrompt,
		User:        buildPrompt(in),
		Temperature: 0,
		JSONSchema:  json.RawMessage(extractionSchema),
	})
	if err != nil {
		return memory.GraphExtraction{}, fmt.Errorf("graph extract: %w", err)
	}

	var raw rawExtraction
	if err := json.Unmarshal([]byte(stripFences(reply)), &raw); err != nil {
		// La réponse n'est jamais journalisée ni renvoyée: elle porte du
		// contenu de message. Sa longueur suffit à distinguer une réponse
		// vide d'une réponse hors format.
		return memory.GraphExtraction{}, fmt.Errorf(
			"graph extract: unparsable model reply of %d bytes: %w", len(reply), err)
	}

	return e.convert(in, raw), nil
}

// convert résout les temp_id, calcule les clés déterministes et rattache
// tout au workspace et à la conversation. Rien de ce que le modèle a
// produit n'est cru sur parole: le workspace vient de l'entrée, jamais de
// la réponse, et les sources sont filtrées sur la fenêtre autorisée.
func (e *Extractor) convert(in memory.GraphExtractorInput,
	raw rawExtraction) memory.GraphExtraction {

	// Les clés canoniques déjà connues du workspace: le modèle peut les
	// reprendre, mais pas en inventer une qui pointerait sur une entité
	// existante sans lui ressembler.
	known := make(map[string]bool, len(in.CandidateEntities))
	for _, c := range in.CandidateEntities {
		known[c.CanonicalKey] = true
	}

	// La fenêtre de messages autorisée comme source. Un identifiant hors
	// de cette fenêtre est écarté: la règle d'accès du graphe se dérive
	// des sources, donc une source inventée rattacherait une relation à
	// une conversation que le demandeur n'a pas le droit de lire.
	allowed := map[uuid.UUID]bool{in.Message.MessageID: true}
	for _, m := range in.Context {
		allowed[m.MessageID] = true
	}

	out := memory.GraphExtraction{
		WorkspaceID:    in.WorkspaceID,
		ConversationID: in.ConversationID,
	}
	byTempID := make(map[string]uuid.UUID, len(raw.Entities))

	for _, re := range raw.Entities {
		name := strings.TrimSpace(re.DisplayName)
		if name == "" || strings.TrimSpace(re.EntityType) == "" {
			continue
		}

		key := ""
		switch {
		case re.CanonicalKey != nil && known[*re.CanonicalKey]:
			key = *re.CanonicalKey
		case re.Resolved:
			key = CanonicalKey(re.EntityType, name)
		default:
			key = UnresolvedKey(re.EntityType, name, in.ConversationID)
		}
		// Un nom réduit à de la ponctuation produirait une clé sans partie
		// nominale, donc une entité fourre-tout par type. On l'écarte.
		if strings.HasSuffix(key, ":") {
			continue
		}

		id := EntityID(in.WorkspaceID, key)
		byTempID[re.TempID] = id
		out.Entities = append(out.Entities, memory.GraphEntity{
			EntityID:     id,
			WorkspaceID:  in.WorkspaceID,
			CanonicalKey: key,
			EntityType:   strings.ToLower(strings.TrimSpace(re.EntityType)),
			DisplayName:  name,
			Aliases:      cleanAliases(re.Aliases),
			Resolved:     re.Resolved,
		})
	}

	for _, rr := range raw.Relations {
		source, ok := byTempID[rr.SourceTempID]
		if !ok {
			continue
		}

		var target *uuid.UUID
		if rr.TargetTempID != nil {
			if id, ok := byTempID[*rr.TargetTempID]; ok {
				target = &id
			}
		}
		literal := ""
		if rr.TargetLiteral != nil {
			literal = strings.TrimSpace(*rr.TargetLiteral)
		}
		// On ne tranche pas ici quand le modèle renseigne les deux à la
		// fois: c'est une réponse ambiguë, pas une réponse à corriger, et
		// Validate écarte déjà toute relation qui porte à la fois une
		// cible entité et un littéral (règle "exactement un des deux").

		observed := parseTime(rr.ObservedAt)
		if observed == nil {
			// La date du message est la meilleure valeur par défaut, et
			// c'est ce que la spec appelle observed_at en 4.8.
			at := in.Message.CreatedAt
			observed = &at
		}

		sources := filterSources(rr.SourceMessageIDs, allowed)
		if len(sources) == 0 {
			sources = []uuid.UUID{in.Message.MessageID}
		}

		rtype := NormalizeRelationType(rr.RelationType)
		validFrom := parseTime(rr.ValidFrom)
		dk := DedupKey(source, rtype, target, literal, validFrom)

		out.Relations = append(out.Relations, memory.GraphRelation{
			RelationID:       RelationID(in.WorkspaceID, dk),
			WorkspaceID:      in.WorkspaceID,
			SourceEntityID:   source,
			RelationType:     rtype,
			TargetEntityID:   target,
			TargetLiteral:    literal,
			ObservedAt:       *observed,
			ValidFrom:        validFrom,
			ValidUntil:       parseTime(rr.ValidUntil),
			Confidence:       rr.Confidence,
			DedupKey:         dk,
			ConversationID:   in.ConversationID,
			SourceMessageIDs: sources,
		})
	}

	return out.Validate()
}

// pronounsAndDeterminers liste les mots qui ne sont jamais un alias
// d'entité, quoi que le modèle en dise. Un alias est un autre nom de la
// chose, pas la façon dont une phrase y renvoie.
//
// Ce n'est pas de la cosmétique. L'évaluation a montré qu'une entité
// « tomates » s'était vu attribuer l'alias « elles », et que
// word_similarity('elles', 'Quelle est la capitale de la Mongolie ?') vaut
// 0,429, au-dessus du seuil de détection de graines. Un pronom stocké en
// alias fait donc semer le graphe sur des questions sans aucun rapport,
// avec de bons rangs, et c'est ce qui empêchait une requête à laquelle rien
// ne devait répondre de ne rien rendre. Les jetons courts et fréquents sont
// les pires collisionneurs de trigrammes.
var pronounsAndDeterminers = map[string]bool{
	"je": true, "tu": true, "il": true, "elle": true, "on": true,
	"nous": true, "vous": true, "ils": true, "elles": true,
	"me": true, "te": true, "se": true, "moi": true, "toi": true, "soi": true,
	"lui": true, "leur": true, "leurs": true, "en": true, "y": true,
	"ce": true, "cet": true, "cette": true, "ces": true, "ca": true,
	"cela": true, "ceci": true, "celui": true, "celle": true,
	"ceux": true, "celles": true,
	"mon": true, "ma": true, "mes": true, "ton": true, "ta": true, "tes": true,
	"son": true, "sa": true, "ses": true, "notre": true, "nos": true,
	"votre": true, "vos": true,
	"le": true, "la": true, "les": true, "un": true, "une": true, "des": true,
	"du": true, "de": true, "au": true, "aux": true,
	"qui": true, "que": true, "quoi": true, "dont": true, "ou": true,
	"the": true, "it": true, "they": true, "them": true, "its": true,
}

// cleanAliases écarte ce qui ne peut pas identifier une entité: les pronoms,
// les déterminants, et les jetons d'un seul caractère. Le reste passe tel
// quel, y compris les sigles courts comme « RH » ou « PP2X », qui sont de
// vrais alias.
func cleanAliases(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range in {
		a = strings.TrimSpace(a)
		key := strings.ToLower(strings.Trim(a, "'\u2019.,;:!?"))
		if key == "" || len([]rune(key)) < 2 || pronounsAndDeterminers[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// buildPrompt assemble l'entrée décrite en section 7.1: le message neuf,
// son contexte immédiat, les identités de la conversation et les entités
// candidates. Les identifiants des messages figurent en clair parce que le
// modèle doit pouvoir les citer dans source_message_ids.
func buildPrompt(in memory.GraphExtractorInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Conversation: %s\n", in.ConversationID)
	if len(in.Participants) > 0 {
		fmt.Fprintf(&b, "Participants: %s\n", strings.Join(in.Participants, ", "))
	}

	if len(in.CandidateEntities) > 0 {
		b.WriteString("\nEntités connues du workspace:\n")
		for _, c := range in.CandidateEntities {
			fmt.Fprintf(&b, "- %s (%s) clé=%s\n",
				c.DisplayName, c.EntityType, c.CanonicalKey)
		}
	}

	if len(in.Context) > 0 {
		b.WriteString("\nContexte précédent:\n")
		for _, m := range in.Context {
			fmt.Fprintf(&b, "[%s] %s (%s): %s\n",
				m.MessageID, m.AuthorKey, m.CreatedAt.UTC().Format(time.RFC3339),
				m.Content)
		}
	}

	b.WriteString("\nMessage à traiter:\n")
	fmt.Fprintf(&b, "[%s] %s (%s): %s\n",
		in.Message.MessageID, in.Message.AuthorKey,
		in.Message.CreatedAt.UTC().Format(time.RFC3339), in.Message.Content)

	return b.String()
}

// stripFences retire les balises de code que certains modèles ajoutent
// autour d'un JSON pourtant demandé en response_format.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

// parseTime accepte du RFC3339 et rend nil sur tout le reste, y compris la
// chaîne vide et le littéral null. Une date illisible n'est pas une erreur:
// l'appelant retombe sur une valeur par défaut sensée.
func parseTime(s *string) *time.Time {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*s))
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// filterSources ne garde que les identifiants de la fenêtre soumise au
// modèle. C'est une frontière de sécurité et pas seulement une validation
// de forme: l'accès aux relations se dérive de leurs sources.
func filterSources(ids []string, allowed map[uuid.UUID]bool) []uuid.UUID {
	var out []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, s := range ids {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil || !allowed[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

var _ memory.GraphExtractor = (*Extractor)(nil)
