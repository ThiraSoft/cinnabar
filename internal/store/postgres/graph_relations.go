package postgres

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/ThiraSoft/cinnabar/internal/graph"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// upsertRelationSQL applique la déduplication de la section 7.3: sur
// conflit de dedup_key, pas de nouvelle ligne, et on garde la confiance la
// plus élevée. La source, elle, s'ajoute à part dans
// graph_relation_sources, ce qui fait qu'un fait répété dans dix messages
// est une relation et dix sources.
//
// valid_until n'est pas écrit ici. Pour un type à valeur unique il sera
// posé par le recalcul de chaîne juste après, et pour les autres types il
// vient de l'extracteur, écrit à l'insertion et jamais retouché.
const upsertRelationSQL = `
INSERT INTO graph_relations
	(relation_id, workspace_id, source_entity_id, relation_type,
	 target_entity_id, target_literal, observed_at, valid_from, valid_until,
	 confidence, scope, dedup_key, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (workspace_id, dedup_key) DO UPDATE SET
	confidence = GREATEST(graph_relations.confidence, EXCLUDED.confidence),
	-- L'embedding n'est écrit que s'il manque: le fait dédupliqué est le
	-- même, donc son vecteur aussi, et le réécrire à chaque source ajoutée
	-- serait du travail pour rien. Mais une relation écrite pendant une
	-- panne de l'embedder doit pouvoir recevoir son vecteur plus tard.
	embedding = COALESCE(graph_relations.embedding, EXCLUDED.embedding)
RETURNING relation_id`

// insertSourceSQL rattache un message à une relation. ON CONFLICT DO
// NOTHING rend le rejeu d'un job d'extraction inoffensif.
const insertSourceSQL = `
INSERT INTO graph_relation_sources (relation_id, message_id, conversation_id)
VALUES ($1, $2, $3)
ON CONFLICT (relation_id, message_id) DO NOTHING`

// Apply écrit une extraction entière dans une seule transaction: entités,
// relations, sources, puis recalcul des chaînes de validité des couples
// touchés qui sont à valeur unique.
//
// Le tout ou rien compte ici. Une extraction à moitié écrite laisserait des
// relations sans leurs sources, donc invisibles à la recherche puisque
// l'accès se dérive des sources, et le rejeu du job les recréerait sans
// jamais réparer les premières.
func (r *GraphRepo) Apply(ctx context.Context, e memory.GraphExtraction,
	singleValued []string) error {

	e = e.Validate()
	if len(e.Entities) == 0 && len(e.Relations) == 0 {
		return nil
	}

	// Le scope est vérifié avant d'ouvrir quoi que ce soit. La colonne est
	// NOT NULL, mais une chaîne vide n'est pas NULL et passerait donc la
	// contrainte.
	//
	// Ce que ce garde protège, et il faut le dire exactement, parce que la
	// version précédente de ce commentaire disait faux: l'accès du graphe ne
	// dépend pas de cette colonne. graphSQL ne lit jamais rel.scope, il dérive
	// l'accès entièrement des sources, par graph_relation_sources vers
	// messages puis ConversationGuardJoinOnMessages vers readable et
	// memory_unit_acl. La colonne n'apparaît dans aucune requête de lecture du
	// paquet. Elle est écrite pour l'audit et pour un lecteur futur, pas pour
	// une garantie d'accès qu'elle ne porte pas.
	//
	// Le garde reste bon pour autant, mais pour une autre raison: l'extracteur
	// ne renseigne pas ce champ, c'est au handler de le poser depuis la
	// conversation, si bien qu'un scope vide est un défaut de câblage et non du
	// bruit de modèle. Échouer bruyamment sur un défaut de câblage vaut mieux
	// que d'écrire une colonne d'audit vide en silence. Contrairement à ce
	// qu'écarte Validate, ça se signale.
	for _, rel := range e.Relations {
		if rel.Scope == "" {
			// La ligne est identifiée par son relation_id et rien d'autre.
			// relation_type sort du modèle d'extraction, donc du contenu des
			// messages, et cette erreur remonte jusqu'à un slog.Warn du runner
			// et jusqu'à jobs.last_error, une colonne durable qu'aucune
			// politique de rétention du contenu des messages ne couvre. Le
			// dedup_key localiserait tout aussi bien mais porte le type et le
			// littéral, donc du contenu lui aussi.
			return fmt.Errorf("apply graph: empty scope for relation %s",
				rel.RelationID)
		}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("apply graph: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := upsertEntitiesTx(ctx, tx, e.Entities); err != nil {
		return fmt.Errorf("apply graph: %w", err)
	}

	// Les couples à recalculer sont connus avant toute écriture: ils se
	// lisent sur e.Relations et pas sur ce que la base rend. Ils sont traités
	// une seule fois chacun après toutes les insertions, parce que recalculer
	// à chaque relation ferait autant de passes que d'observations, avec des
	// états intermédiaires incohérents entre elles.
	touched := map[chainPair]bool{}
	for _, rel := range e.Relations {
		if graph.IsSingleValued(singleValued, rel.RelationType) {
			touched[chainPair{e.WorkspaceID, rel.SourceEntityID, rel.RelationType}] = true
		}
	}

	// Les chaînes sont verrouillées avant les upserts, dans l'ordre de
	// sortedChainPairs, c'est-à-dire exactement l'ordre où Reevaluate les
	// prend. Sans ce pré-verrouillage, Apply prenait d'abord les lignes
	// qu'elle réécrit et seulement ensuite le reste de leur couple: une
	// extraction qui ne réaffirme qu'une observation sur deux verrouillait
	// donc la seconde ligne du couple avant la première, dans l'ordre inverse
	// de Reevaluate, et les deux partaient en 40P01. Mesuré: 24 interblocages
	// sur 60 rondes avec les seuls tris, 0 avec ce pré-verrouillage.
	//
	// Le recalcul qui suit refait le même FOR UPDATE, et il le faut: les
	// lignes insérées entre-temps ne sont pas dans l'instantané de celui-ci.
	// Ce qu'on prend ici, ce ne sont pas des données, ce sont des verrous.
	for _, p := range sortedChainPairs(touched) {
		if err := lockChainTx(ctx, tx, p.workspace, p.source, p.rtype); err != nil {
			return fmt.Errorf("apply graph: %w", err)
		}
	}

	for _, rel := range sortedRelations(e.Relations) {
		var relationID uuid.UUID
		if err := tx.QueryRow(ctx, upsertRelationSQL,
			rel.RelationID, rel.WorkspaceID, rel.SourceEntityID, rel.RelationType,
			rel.TargetEntityID, nullIfEmpty(rel.TargetLiteral), rel.ObservedAt,
			rel.ValidFrom, rel.ValidUntil, rel.Confidence, rel.Scope, rel.DedupKey,
			factEmbedding(rel.Embedding),
		).Scan(&relationID); err != nil {
			return fmt.Errorf("apply graph: upsert relation: %w", err)
		}

		for _, msgID := range rel.SourceMessageIDs {
			if _, err := tx.Exec(ctx, insertSourceSQL,
				relationID, msgID, rel.ConversationID); err != nil {
				return fmt.Errorf("apply graph: insert source: %w", err)
			}
		}
	}

	for _, p := range sortedChainPairs(touched) {
		if err := recomputeChainTx(ctx, tx, p.workspace, p.source, p.rtype); err != nil {
			return fmt.Errorf("apply graph: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("apply graph: commit: %w", err)
	}
	return nil
}

// chainSelectSQL lit toutes les observations non invalidées d'un couple
// (entité source, type).
//
// Ce n'est pas FOR UPDATE qui sérialise deux Apply concurrents sur le même
// sujet, contrairement à ce qu'on croit en le lisant. FOR UPDATE ne
// verrouille que les lignes visibles dans l'instantané de la transaction,
// donc il ne voit pas, et ne peut pas bloquer sur, une observation qu'une
// autre transaction vient d'insérer sans avoir commité. Mesuré: deux
// transactions insérant chacune une observation neuve du même couple voient
// chacune la sienne et aucune ne bloque.
//
// Ce qui les sérialise vraiment est trois fichiers plus loin: Validate
// impose que l'entité source d'une relation figure dans l'extraction, et
// Apply fait passer upsertEntitiesTx en tout premier, dont l'INSERT ...
// ON CONFLICT DO UPDATE prend un verrou de ligne exclusif sur l'entité.
// Deux Apply sur le même couple upsertent donc forcément la même ligne
// d'entité avant toute autre chose, et la seconde attend le commit de la
// première. Déplacer l'upsert des entités après les relations, ou relâcher
// ce test dans Validate, casserait la sérialisation sans que FOR UPDATE ne
// rattrape quoi que ce soit.
//
// FOR UPDATE n'est pas inutile pour autant: il fait ce travail entre
// Reevaluate et Apply, puisque Reevaluate ne touche aucune ligne d'entité
// et n'a donc pas ce garde-fou. C'est aussi pourquoi les couples sont
// verrouillés dans un ordre trié (voir sortedChainPairs), les entités aussi
// (voir upsertEntitiesTx) et les relations d'une extraction aussi (voir
// sortedRelations): un ordre global est ce qui empêche deux transactions de
// se croiser en sens inverse et de partir en 40P01.
//
// L'ORDER BY est relation_id et non observed_at, et c'est un ordre de
// verrouillage et pas un ordre de lecture. observed_at ne donnait pas un
// ordre total: deux observations simultanées sortaient dans un ordre que rien
// ne fixe, et surtout il ne pouvait pas coïncider avec celui de
// sortedRelations, qui n'a que des identifiants à sa disposition au moment de
// l'upsert. graph.Chain retrie par observed_at en Go, donc rien de la logique
// de chaîne ne dépend de l'ordre rendu ici; l'ordre des mises à jour de
// chainUpdateSQL change, pas leurs valeurs.
//
// relation_type est comparé en égalité exacte, alors que IsSingleValued
// normalise ses deux côtés. Ça tient parce que l'extracteur n'écrit que du
// NormalizeRelationType; deux graphies du même prédicat en base formeraient
// deux chaînes séparées qui ne se fermeraient jamais l'une l'autre.
const chainSelectSQL = `
SELECT relation_id, observed_at, valid_until
FROM graph_relations
WHERE workspace_id = $1
  AND source_entity_id = $2
  AND relation_type = $3
  AND invalidated_at IS NULL
ORDER BY relation_id
FOR UPDATE`

const chainUpdateSQL = `
UPDATE graph_relations SET valid_until = $2 WHERE relation_id = $1`

// lockChainTx prend les verrous de ligne d'un couple sans rien en lire. Elle
// existe pour que Apply puisse aligner son ordre de verrouillage sur celui de
// Reevaluate avant d'écrire quoi que ce soit, et elle partage forcément
// chainSelectSQL avec recomputeChainTx: deux requêtes distinctes finiraient
// par verrouiller deux ensembles de lignes différents, ce qui est précisément
// la faute qu'elle corrige.
func lockChainTx(ctx context.Context, tx pgx.Tx,
	workspaceID string, source uuid.UUID, relationType string) error {

	rows, err := tx.Query(ctx, chainSelectSQL, workspaceID, source, relationType)
	if err != nil {
		return fmt.Errorf("lock chain: %w", err)
	}
	for rows.Next() {
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lock chain: %w", err)
	}
	return nil
}

// recomputeChainTx recalcule intégralement les fenêtres de validité d'un
// couple à valeur unique. Voir graph.Chain pour pourquoi le recalcul est
// complet et non incrémental.
func recomputeChainTx(ctx context.Context, tx pgx.Tx,
	workspaceID string, source uuid.UUID, relationType string) error {

	rows, err := tx.Query(ctx, chainSelectSQL, workspaceID, source, relationType)
	if err != nil {
		return fmt.Errorf("recompute chain: %w", err)
	}
	var obs []graph.Observation
	for rows.Next() {
		var o graph.Observation
		if err := rows.Scan(&o.RelationID, &o.ObservedAt, &o.ValidUntil); err != nil {
			rows.Close()
			return fmt.Errorf("recompute chain: scan: %w", err)
		}
		obs = append(obs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("recompute chain: %w", err)
	}

	for _, o := range graph.Chain(obs) {
		if _, err := tx.Exec(ctx, chainUpdateSQL, o.RelationID, o.ValidUntil); err != nil {
			return fmt.Errorf("recompute chain: update: %w", err)
		}
	}
	return nil
}

// touchedByMessageSQL rend les couples que la disparition d'un message peut
// avoir déplacés, avec la relation concernée.
const touchedByMessageSQL = `
SELECT DISTINCT r.relation_id, r.workspace_id, r.source_entity_id, r.relation_type
FROM graph_relations r
JOIN graph_relation_sources s ON s.relation_id = r.relation_id
WHERE s.message_id = $1`

// lockRelationsSQL prend les verrous de ligne des relations données, dans
// l'ordre exact du tableau reçu, sans rien en lire d'utile.
//
// Elle existe pour que Reevaluate verrouille les lignes que son UPDATE
// d'invalidation va toucher avant de les toucher, et dans un ordre choisi
// plutôt que subi. invalidateSQL les verrouillait dans l'ordre que son plan
// d'exécution produisait: ni celui des couples, ni celui de relation_id, ni
// aucun ordre que quiconque ait décidé, et donc pas celui de sortedRelations
// que suit Apply. Le plan de cet UPDATE, vu par EXPLAIN, est un
// Hash Right Anti Join au-dessus d'un Seq Scan sur graph_relations; il n'a
// aucune raison de rester celui-là quand la table grossit ou que les
// statistiques changent, et c'est bien le problème: l'ordre de verrouillage
// suivait une décision du planificateur.
//
// Mesuré à 29 interblocages sur 60 rondes avant ce pré-verrouillage et 0
// après, par TestSondeDeChargeApplyEtReevaluateSurUneInvalidation. Cette
// mesure documente le défaut, elle ne le garde pas: la sonde est sur une pente
// et son propre commentaire dit pourquoi. Ce qui garde cette propriété est
// graph_lockorder_test.go, qui l'épingle par le mécanisme.
//
// L'ORDER BY porte sur array_position et pas sur les colonnes, exprès. Trier
// côté SQL par (source_entity_id, relation_type, relation_id) donnerait
// presque le même ordre, mais relation_type est du texte: son ordre dépendrait
// de la collation de la base alors que sortedRelations et sortedChainPairs
// comparent des octets en Go. Passer l'ordre déjà décidé en Go supprime la
// question au lieu de parier sur une collation.
//
// FOR UPDATE au-dessus d'un ORDER BY verrouille bien dans l'ordre trié: le
// nœud LockRows est au-dessus du nœud Sort dans le plan. Vérifié par EXPLAIN
// et non supposé:
//
//	LockRows
//	  ->  Sort
//	        Sort Key: (array_position('{...}'::uuid[], relation_id))
//	        ->  <accès à graph_relations>
//
// Seule la forme des deux nœuds du haut compte, et elle ne dépend pas des
// statistiques: c'est la même propriété dont dépend l'ORDER BY de
// chainSelectSQL. Le nœud d'accès, en dessous, varie: Seq Scan sur une table
// de 120 lignes analysée, Bitmap Heap Scan sur une table vide. Il ne faut donc
// rien conclure de lui, et un commentaire précédent avait tort de citer le
// second comme s'il était la règle.
const lockRelationsSQL = `
SELECT relation_id
FROM graph_relations
WHERE relation_id = ANY($1)
ORDER BY array_position($1, relation_id)
FOR UPDATE`

// invalidateSQL pose l'invalidated_at des relations dont plus aucune source
// n'est vivante. Le NOT EXISTS est la lettre de la section 7.5: tant qu'un
// seul message source survit, la relation reste crue.
//
// Le filtre invalidated_at IS NULL rend l'opération idempotente et
// préserve la date de la première invalidation.
const invalidateSQL = `
UPDATE graph_relations r
SET invalidated_at = now()
WHERE r.relation_id = ANY($1)
  AND r.invalidated_at IS NULL
  AND NOT EXISTS (
	SELECT 1
	FROM graph_relation_sources s
	JOIN messages m ON m.message_id = s.message_id
	WHERE s.relation_id = r.relation_id
	  AND m.deleted_at IS NULL
  )`

// Reevaluate traite l'édition ou la suppression d'un message. Elle
// n'efface rien: une relation dont toutes les sources ont disparu prend un
// invalidated_at, ce qui la sort des lectures sans perdre l'historique.
func (r *GraphRepo) Reevaluate(ctx context.Context, messageID uuid.UUID,
	singleValued []string) error {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("reevaluate graph: %w", err)
	}
	defer tx.Rollback(ctx)

	var sourced []sourcedRelation
	touched := map[chainPair]bool{}

	rows, err := tx.Query(ctx, touchedByMessageSQL, messageID)
	if err != nil {
		return fmt.Errorf("reevaluate graph: %w", err)
	}
	for rows.Next() {
		var r sourcedRelation
		if err := rows.Scan(&r.id, &r.pair.workspace, &r.pair.source,
			&r.pair.rtype); err != nil {
			rows.Close()
			return fmt.Errorf("reevaluate graph: scan: %w", err)
		}
		sourced = append(sourced, r)
		if graph.IsSingleValued(singleValued, r.pair.rtype) {
			touched[r.pair] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reevaluate graph: %w", err)
	}

	// Un message qui ne sourçait rien est le cas courant, pas une erreur:
	// tous les messages ne produisent pas de relation.
	if len(sourced) == 0 {
		return nil
	}

	// L'ordre de verrouillage de Reevaluate doit être celui d'Apply, et il l'est
	// maintenant en deux temps, exactement comme là-bas: d'abord les chaînes
	// des couples touchés dans l'ordre de sortedChainPairs, ensuite les lignes
	// de l'invalidation dans l'ordre de sortedRelations. Les lignes déjà prises
	// par la première étape le sont pour rien la seconde fois, ce qui est
	// gratuit, et celles des types qui ne sont pas à valeur unique n'entrent
	// dans aucune chaîne: c'est pour elles que la seconde étape existe.
	//
	// Sans ça, invalidateSQL prenait ses verrous dans l'ordre de son plan
	// d'exécution, donc dans un ordre que personne n'avait choisi et qui n'a
	// aucune raison d'être celui d'Apply.
	for _, p := range sortedChainPairs(touched) {
		if err := lockChainTx(ctx, tx, p.workspace, p.source, p.rtype); err != nil {
			return fmt.Errorf("reevaluate graph: %w", err)
		}
	}
	ids := sortedSourcedIDs(sourced)
	if _, err := tx.Exec(ctx, lockRelationsSQL, ids); err != nil {
		return fmt.Errorf("reevaluate graph: lock relations: %w", err)
	}

	if _, err := tx.Exec(ctx, invalidateSQL, ids); err != nil {
		return fmt.Errorf("reevaluate graph: invalidate: %w", err)
	}

	// Le recalcul vient après l'invalidation, jamais avant: chainSelectSQL
	// ne lit que les relations non invalidées, donc c'est l'invalidation
	// qui détermine quelles observations entrent dans la chaîne.
	for _, p := range sortedChainPairs(touched) {
		if err := recomputeChainTx(ctx, tx, p.workspace, p.source, p.rtype); err != nil {
			return fmt.Errorf("reevaluate graph: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("reevaluate graph: commit: %w", err)
	}
	return nil
}

// sortedRelations rend les relations dans l'ordre exact où les deux chemins
// d'écriture du graphe verrouillent les lignes de graph_relations: le couple
// (entité source, type) d'abord, dans l'ordre de sortedChainPairs, puis
// relation_id, dans l'ordre de chainSelectSQL.
//
// Sans ce tri, Apply verrouillait ses lignes dans l'ordre du slice
// e.Relations, qui est l'ordre de sortie du modèle et n'est trié par rien,
// pendant que Reevaluate les verrouillait dans l'ordre des couples. Les deux
// prenaient donc des verrous sur les mêmes lignes dans deux ordres
// différents, ce que Postgres tranche par un deadlock detected (40P01):
// mesuré à 59 rondes sur 60 par TestApplyEtReevaluateConcurrentsNInterbloquent
// Pas. Le tri des entités de upsertEntitiesTx ne rattrape rien ici, puisque
// Reevaluate ne prend jamais de verrou d'entité, et c'est exactement pour ça
// que sortedChainPairs existe.
//
// Trier par relation_id seul ne suffit pas, et il faut le dire parce que
// c'est le raccourci qu'on prend naturellement. relation_id est un hachage de
// la clé de déduplication: il n'a aucun rapport avec l'ordre des couples.
// Deux lignes de couples différents seraient donc prises par Apply dans
// l'ordre de leur hachage et par Reevaluate dans l'ordre des couples, une
// fois sur deux en sens inverse. Mesuré sur le test croisé: 59 interblocages
// sur 60 rondes avec le tri par relation_id seul, autant dire aucun progrès,
// contre 24 sur 60 avec celui-ci. Le tri doit être celui du verrouillage, pas
// un ordre total quelconque.
//
// Ces 24 restants sont fermés par le pré-verrouillage des chaînes dans Apply,
// pas par ce tri: voir son commentaire. Le tri reste nécessaire pour les types
// qui ne sont pas à valeur unique, qui n'entrent dans aucune chaîne et n'ont
// donc pas ce pré-verrouillage.
//
// La copie évite de réordonner le slice de l'appelant sous ses pieds, comme
// dans upsertEntitiesTx et pour la même raison.
func sortedRelations(rels []memory.GraphRelation) []memory.GraphRelation {
	out := make([]memory.GraphRelation, len(rels))
	copy(out, rels)
	slices.SortFunc(out, func(a, b memory.GraphRelation) int {
		if c := bytes.Compare(a.SourceEntityID[:], b.SourceEntityID[:]); c != 0 {
			return c
		}
		if c := strings.Compare(a.RelationType, b.RelationType); c != 0 {
			return c
		}
		return bytes.Compare(a.RelationID[:], b.RelationID[:])
	})
	return out
}

// sourcedRelation est une relation qu'un message source, avec le couple dont
// elle fait partie. Reevaluate a besoin des deux: du couple pour savoir quelle
// chaîne recalculer, de l'identifiant pour verrouiller la ligne.
type sourcedRelation struct {
	id   uuid.UUID
	pair chainPair
}

// sortedSourcedIDs rend les identifiants dans l'ordre de verrouillage commun
// aux deux chemins d'écriture: le couple d'abord, comme sortedChainPairs, puis
// relation_id, comme chainSelectSQL. C'est le même ordre que sortedRelations,
// et les deux doivent rester en phase; ils ne peuvent pas partager de code,
// l'un triant des relations à écrire et l'autre des identifiants lus en base.
func sortedSourcedIDs(sourced []sourcedRelation) []uuid.UUID {
	out := make([]sourcedRelation, len(sourced))
	copy(out, sourced)
	slices.SortFunc(out, func(a, b sourcedRelation) int {
		if c := bytes.Compare(a.pair.source[:], b.pair.source[:]); c != 0 {
			return c
		}
		if c := strings.Compare(a.pair.rtype, b.pair.rtype); c != 0 {
			return c
		}
		return bytes.Compare(a.id[:], b.id[:])
	})
	ids := make([]uuid.UUID, len(out))
	for i, r := range out {
		ids[i] = r.id
	}
	return ids
}

// chainPair désigne un couple (entité source, type de relation) dont la
// chaîne de validité est à recalculer, dans son workspace.
type chainPair struct {
	workspace string
	source    uuid.UUID
	rtype     string
}

// sortedChainPairs rend les couples touchés dans un ordre total et stable.
//
// L'ensemble est une map, et Go randomise délibérément le parcours d'une
// map. Chaque recalcul verrouille les lignes de son couple avec FOR UPDATE,
// si bien que deux transactions touchant les deux mêmes couples les
// prenaient dans des ordres tirés au hasard, ce que Postgres tranche par un
// deadlock detected (40P01). Reevaluate est la plus exposée: elle ne
// verrouille aucune ligne d'entité, donc rien d'autre ne la sérialise.
func sortedChainPairs(touched map[chainPair]bool) []chainPair {
	out := make([]chainPair, 0, len(touched))
	for p := range touched {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b chainPair) int {
		if c := strings.Compare(a.workspace, b.workspace); c != 0 {
			return c
		}
		if c := bytes.Compare(a.source[:], b.source[:]); c != 0 {
			return c
		}
		return strings.Compare(a.rtype, b.rtype)
	})
	return out
}

// factEmbedding convertit un vecteur du domaine en valeur pgvector, ou en
// NULL quand il est absent. Nil est un état normal: la relation s'écrit sans
// vecteur et la recherche retombe sur son ordre historique.
func factEmbedding(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	return pgvector.NewVector(v)
}

// nullIfEmpty rend un pointeur nil pour une chaîne vide. Le CHECK de
// graph_relations exige qu'exactement une des deux cibles soit renseignée,
// et une chaîne vide compterait comme renseignée.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var _ memory.GraphRepo = (*GraphRepo)(nil)
