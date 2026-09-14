package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// GraphRepo porte l'écriture et la maintenance du graphe. La lecture de
// recherche vit dans SearchRepo, comme pour le dense et le lexical:
// l'écriture et la recherche n'ont ni le même cycle de vie ni les mêmes
// contraintes d'accès.
type GraphRepo struct {
	pool *pgxpool.Pool
}

func NewGraphRepo(pool *pgxpool.Pool) *GraphRepo {
	return &GraphRepo{pool: pool}
}

// upsertEntitySQL déduplique par l'index unique partiel
// graph_entities_canonical_idx. Le prédicat WHERE de l'index doit être
// répété dans le ON CONFLICT, sinon Postgres refuse de l'utiliser comme
// arbitre.
//
// display_name n'est jamais écrasé: la première graphie observée fait foi,
// et la nouvelle rejoint les alias. Sinon deux extractions concurrentes
// feraient osciller le nom d'une même entité d'un appel à l'autre.
//
// resolved ne peut que monter. Une entité résolue ne se déresout pas parce
// qu'une extraction ultérieure a hésité; la fusion de deux entités reste
// hors périmètre (règle 7.3 de la spec), donc rien ne doit pouvoir
// dégrader ce qui a été résolu.
const upsertEntitySQL = `
INSERT INTO graph_entities
	(entity_id, workspace_id, canonical_key, entity_type,
	 display_name, aliases, resolved)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, canonical_key) WHERE canonical_key IS NOT NULL
DO UPDATE SET
	aliases = COALESCE((
		SELECT jsonb_agg(DISTINCT a)
		FROM jsonb_array_elements_text(
			graph_entities.aliases
			|| EXCLUDED.aliases
			|| CASE WHEN EXCLUDED.display_name = graph_entities.display_name
			        THEN '[]'::jsonb
			        ELSE jsonb_build_array(EXCLUDED.display_name) END
		) AS t(a)
	), '[]'::jsonb),
	resolved   = graph_entities.resolved OR EXCLUDED.resolved,
	updated_at = now()
RETURNING entity_id`

// UpsertEntities écrit les entités dans leur propre transaction. Utilisée
// telle quelle par les tests et par CandidateEntities; Apply appelle la
// variante transactionnelle pour tout écrire d'un bloc.
func (r *GraphRepo) UpsertEntities(ctx context.Context, ents []memory.GraphEntity) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("upsert entities: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := upsertEntitiesTx(ctx, tx, ents); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("upsert entities: commit: %w", err)
	}
	return nil
}

// upsertEntitiesTx écrit les entités dans une transaction fournie.
//
// Le RETURNING est lu puis jeté, et c'est délibéré: il force l'upsert à
// rendre une ligne, donc à échouer bruyamment si le ON CONFLICT ne
// trouvait pas son arbitre, au lieu de passer en silence. L'identifiant
// rendu est toujours celui qui a été envoyé, EntityID étant déterministe
// sur (workspace, canonical_key), c'est-à-dire exactement la clé de
// conflit. Rien ne dépend donc de sa valeur, et les relations peuvent
// pointer sur l'identifiant calculé côté Go.
func upsertEntitiesTx(ctx context.Context, tx pgx.Tx, ents []memory.GraphEntity) error {
	// L'ordre de verrouillage doit être global, pas celui du slice. C'est ici
	// que la toute première ligne d'une extraction est verrouillée, par le
	// verrou exclusif que l'INSERT ... ON CONFLICT DO UPDATE prend sur la
	// ligne d'entité, et c'est donc ici que deux extractions concurrentes se
	// croisent. Deux messages qui parlent de la tomate puis du basilic et du
	// basilic puis de la tomate se verrouillaient dans des ordres opposés, ce
	// que Postgres tranche par un deadlock detected (40P01). Trier par
	// entity_id donne à tout le monde le même ordre et supprime la classe.
	//
	// La copie évite de réordonner le slice de l'appelant sous ses pieds.
	sorted := make([]memory.GraphEntity, len(ents))
	copy(sorted, ents)
	slices.SortFunc(sorted, func(a, b memory.GraphEntity) int {
		return bytes.Compare(a.EntityID[:], b.EntityID[:])
	})

	for _, e := range sorted {
		if e.CanonicalKey == "" {
			// L'entité est identifiée par son entity_id, jamais par son
			// display_name: celui-ci sort du modèle d'extraction, donc du
			// contenu des messages, et cette erreur remonte jusqu'à un
			// slog.Warn du runner et jusqu'à jobs.last_error, une colonne
			// durable qu'aucune politique de rétention du contenu des messages
			// ne couvre.
			return fmt.Errorf("upsert entities: empty canonical key for entity %s",
				e.EntityID)
		}
		aliases, err := json.Marshal(nonNilStrings(e.Aliases))
		if err != nil {
			return fmt.Errorf("upsert entities: encode aliases: %w", err)
		}
		var got uuid.UUID
		if err := tx.QueryRow(ctx, upsertEntitySQL,
			e.EntityID, e.WorkspaceID, e.CanonicalKey, e.EntityType,
			e.DisplayName, aliases, e.Resolved).Scan(&got); err != nil {
			return fmt.Errorf("upsert entities: %w", err)
		}
	}
	return nil
}

// candidateEntitiesSQL alimente le prompt de l'extracteur. Le texte donné
// est un message entier, pas juste un nom: on cherche donc si le nom
// d'affichage apparaît comme une portion du texte, avec word_similarity,
// plutôt qu'avec similarity qui compare les deux chaînes dans leur
// ensemble et ne retrouverait jamais "Paul Dupont" dans "est-ce que Paul
// Dupon aime les tomates".
//
// Cette direction n'est pas indexable et il faut le savoir: l'index
// trigramme sur display_name accélère la recherche d'une aiguille dans une
// colonne longue, pas d'une colonne courte à l'intérieur d'un texte long.
// La revue de la tâche 4 l'a vérifié dans pg_amop et par EXPLAIN sur
// 150 000 lignes avec enable_seqscan désactivé: toujours un parcours
// séquentiel. Ce qui borne le coût, c'est graph_entities_workspace_idx
// (migration 004), qui restreint le parcours aux entités du workspace au
// lieu de celles de toute la plateforme.
//
// Le coût reste acceptable parce que cette requête vit sur le chemin d'un
// worker d'extraction, une fois par message ingéré, jamais sur le chemin
// d'une recherche.
//
// La recherche porte sur le nom d'affichage et sur les alias. Elle est
// bornée au workspace: une entité d'un autre tenant ne doit jamais entrer
// dans un prompt.
const candidateEntitiesSQL = `
SELECT entity_id, workspace_id, canonical_key, entity_type,
       display_name,
       ARRAY(SELECT jsonb_array_elements_text(aliases)),
       resolved
FROM graph_entities
WHERE workspace_id = $1
  AND (
	word_similarity(display_name, $2) >= $4
	OR EXISTS (
		SELECT 1 FROM jsonb_array_elements_text(aliases) AS t(a)
		WHERE word_similarity(t.a, $2) >= $4
	)
  )
ORDER BY word_similarity(display_name, $2) DESC, updated_at DESC
LIMIT $3`

// candidateSimilarityThreshold est le plancher de word_similarity au-dessus
// duquel une entité entre dans le prompt d'extraction.
//
// Le seuil est passé en paramètre plutôt que laissé à l'opérateur <%, qui
// lit word_similarity_threshold, un réglage de session. Un réglage global
// invisible depuis cette requête finirait par diverger entre deux
// déploiements, et personne ne saurait pourquoi le rappel change.
//
// 0,4 vient de mesures sur du français, pas d'une intuition. Les variantes
// de graphie que le seuil par défaut de 0,6 laissait tomber en silence:
// Müller contre Muller donne 0,43, Élodie contre elodie 0,57, François
// contre Francois 0,50. Les vrais négatifs, eux, sont très loin en dessous:
// un nom sans rapport avec la phrase tourne autour de 0,13. La marge entre
// les deux populations est d'un facteur trois, donc le seuil peut descendre
// sans faire entrer du bruit.
const candidateSimilarityThreshold = 0.4

// CandidateEntities rend les entités du workspace qui ressemblent au texte
// donné. Le texte est celui du message en cours d'extraction, jamais celui
// d'un autre workspace.
func (r *GraphRepo) CandidateEntities(ctx context.Context,
	workspaceID, text string, limit int) ([]memory.GraphEntity, error) {

	if workspaceID == "" {
		return nil, fmt.Errorf("candidate entities: empty workspace id")
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 30
	}

	rows, err := r.pool.Query(ctx, candidateEntitiesSQL,
		workspaceID, text, limit, candidateSimilarityThreshold)
	if err != nil {
		return nil, fmt.Errorf("candidate entities: %w", err)
	}
	defer rows.Close()

	var out []memory.GraphEntity
	for rows.Next() {
		var e memory.GraphEntity
		if err := rows.Scan(&e.EntityID, &e.WorkspaceID, &e.CanonicalKey,
			&e.EntityType, &e.DisplayName, &e.Aliases, &e.Resolved); err != nil {
			return nil, fmt.Errorf("candidate entities: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("candidate entities: %w", err)
	}
	return out, nil
}

// nonNilStrings évite qu'un slice nil se sérialise en "null" plutôt qu'en
// tableau vide: la colonne aliases est NOT NULL avec un défaut '[]', et un
// "null" JSON la ferait échouer sur la concaténation jsonb.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
