package postgres

import "fmt"

// ReadableCTE liste les conversations lisibles par le demandeur via leur
// scope (workspace ou participants). C'est la seule définition de la partie
// de la règle d'accès du service qui dépend du scope: les trois stratégies
// de recherche la préfixent, ce qui rend le critère 4 vérifiable en un
// endroit.
//
// $1 = workspace_id du demandeur, $2 = requester_key.
//
// Le scope explicit et le scope private ne figurent pas ici: ils passent
// uniquement par memory_unit_acl, ajouté en OR au niveau des unités par
// chaque stratégie. C'est pour ça que la participation seule n'ouvre le
// EXISTS que pour le scope 'participants', jamais pour 'private' ou
// 'explicit': un participant d'une conversation privée n'a pas
// automatiquement le droit d'en lire les souvenirs, il lui faut une ligne
// memory_unit_acl.
const ReadableCTE = `
readable AS (
	SELECT c.conversation_id, c.scope
	FROM conversations c
	WHERE c.workspace_id = $1
	  AND c.deleted_at IS NULL
	  AND (
		c.scope = 'workspace'
		OR (
			c.scope = 'participants'
			AND EXISTS (
				SELECT 1 FROM conversation_participants p
				WHERE p.conversation_id = c.conversation_id
				  AND p.participant_key = $2
				  AND p.left_at IS NULL
			)
		)
	  )
)`

// conversationGuardPredicate porte les deux prédicats que toute jointure de
// garde doit appliquer sur la conversation, quelle que soit la table de
// départ de la stratégie: son workspace_id doit correspondre à celui du
// demandeur et elle ne doit pas être supprimée. C'est l'unique définition de
// ces deux prédicats; ConversationGuardJoin et
// ConversationGuardJoinOnMessages ne diffèrent que par l'alias auquel ils
// joignent conversation_id, jamais par ces prédicats eux-mêmes.
//
// $1 = workspace_id du demandeur, le même paramètre que ReadableCTE.
const conversationGuardPredicate = `
	AND c.workspace_id = $1
	AND c.deleted_at IS NULL`

// ConversationGuardJoin est le join que toute stratégie partant de
// FROM memory_units mu doit ajouter, en plus de ReadableCTE. Il rend le
// filtre de workspace et de suppression douce inconditionnel plutôt que
// dépendant de la branche empruntée: readable ne couvre que la branche scope
// de la règle d'accès (workspace / participants), pas la branche ACL
// explicite (memory_unit_acl). Sans ce join, une unité dont la conversation
// est supprimée, ou dont le workspace_id diverge de celui de sa conversation,
// pourrait être rendue à quiconque détient une ligne memory_unit_acl dessus,
// sans jamais passer par readable.
//
// memory_units.conversation_id est NOT NULL avec une contrainte de clé
// étrangère vers conversations: une jointure interne ne peut donc jamais
// faire disparaître une unité qui a légitimement une conversation.
//
// Voir ConversationGuardJoinOnMessages, son pendant pour une stratégie
// partant de FROM messages m: les deux doivent rester en phase sur les
// prédicats de conversationGuardPredicate. Une troisième stratégie partant
// d'une autre table a besoin d'une troisième constante du même genre, jamais
// d'une copie modifiée de celle-ci ou de sa sœur.
const ConversationGuardJoin = `
JOIN conversations c
	ON c.conversation_id = mu.conversation_id` + conversationGuardPredicate

// ConversationGuardJoinOnMessages est le pendant de ConversationGuardJoin
// pour une stratégie partant de FROM messages m plutôt que de
// FROM memory_units mu (la recherche lexicale, par exemple, qui doit rester
// trouvable pour un message pas encore vectorisé et ne peut donc pas partir
// de memory_units). Il applique les deux mêmes prédicats de
// conversationGuardPredicate, en les rendant tout aussi inconditionnels:
// sans lui, la branche memory_unit_acl du WHERE d'une telle stratégie
// pourrait rendre un message dont la conversation est supprimée, ou dont le
// workspace_id diverge de celui de sa conversation, par la seule présence
// d'une ligne memory_unit_acl sur l'unité qu'il ancre.
//
// messages.conversation_id est NOT NULL avec une contrainte de clé étrangère
// vers conversations: une jointure interne ne peut donc jamais faire
// disparaître un message qui a légitimement une conversation.
const ConversationGuardJoinOnMessages = `
JOIN conversations c
	ON c.conversation_id = m.conversation_id` + conversationGuardPredicate

// accessReasonExpr calcule la raison d'accès à afficher dans la réponse.
// L'alias attendu est r pour readable: le CASE lit r.scope et
// r.conversation_id, donc l'alias de jointure doit être r dans toute
// stratégie qui l'utilise. Un alias différent ne casse rien à la
// compilation mais produit des raisons d'accès fausses en silence. La
// branche ACL explicite ne dépend d'aucun alias: c'est le ELSE, atteint
// quand r ne couvre pas la ligne (r.conversation_id est alors NULL), ce qui
// ne peut arriver que si la ligne a été rendue par l'autre branche du OR,
// celle qui teste une ligne memory_unit_acl.
const accessReasonExpr = `
	CASE
		WHEN r.scope = 'workspace' THEN 'workspace_scope'
		WHEN r.conversation_id IS NOT NULL THEN 'conversation_participant'
		ELSE 'explicit_acl'
	END`

// ValidateRequester rejette les entrées qui n'ont pas de sens à la seule
// frontière d'autorisation du service. Toute stratégie de recherche
// l'appelle avant d'interroger la base, pour qu'une nouvelle stratégie
// n'ait pas à se souvenir de le faire.
//
// Un requester_key vide est le cas dégénéré qui compte: ReadableCTE ne
// référence $2 (requester_key) que dans sa branche 'participants', jamais
// dans sa branche 'workspace'. Une clé vide échouerait donc déjà fermée pour
// les scopes participants, private et explicit, mais continuerait de rendre
// tout le scope workspace du workspace demandé: c'est le seul cas où un
// paramètre vide élargit l'accès plutôt que de le refuser, et il ne doit
// jamais atteindre le SQL.
func ValidateRequester(workspaceID, requesterKey string) error {
	if workspaceID == "" {
		return fmt.Errorf("search: empty workspace id")
	}
	if requesterKey == "" {
		return fmt.Errorf("search: empty requester key")
	}
	return nil
}

// ExplicitACLSourceWindowPredicate borne un fait du graphe à la fenêtre de
// l'unité octroyée, quand c'est une ligne memory_unit_acl et elle seule qui
// ouvre l'accès.
//
// C'est le pendant, pour un fait dérivé, du bornage que Searcher.expand
// applique à un extrait: un octroi memory_unit_acl porte sur une unité et pas
// sur la conversation, donc il ne donne à lire que [start_sequence,
// end_sequence]. Trois verrous tenaient déjà cette règle côté extraits
// (l'expansion bornée, le refus de MergeAdjacent de recoller un extrait
// explicit_acl avec un voisin admis autrement, et l'absence de liste de
// participants), et les faits ne passaient par aucun des trois.
//
// La couture que ça referme: graph.context_messages vaut 4 et
// indexing.previous_messages vaut 2, donc la fenêtre d'extraction est deux
// fois plus large que celle d'une unité, et filterSources admet délibérément
// toute la fenêtre d'extraction comme source. Une relation extraite en
// traitant M4 pouvait donc être sourcée par M0 tout en ressortant par une
// ACL sur l'unité [M2, M4], en emportant un target_literal que le modèle
// avait levé du texte de M0. Un littéral n'est ni un nom d'entité ni une
// distance: ce n'est aucun des canaux que la section 12 accepte.
//
// La règle est « toutes les sources dans la fenêtre », pas « au moins une
// source lisible ». Cette dernière reste juste pour un accès dérivé du scope,
// où toute la conversation est lisible par construction, et c'est pourquoi le
// prédicat ne mord que quand r ne couvre pas la ligne, exactement la
// condition qui produit explicit_acl dans accessReasonExpr.
//
// Une source dans une autre conversation compte comme hors fenêtre: les
// numéros de séquence sont propres à une conversation, et l'unité octroyée
// n'en couvre qu'une. Un fait dédupliqué entre deux conversations reste rendu
// par sa ligne de l'autre conversation si le demandeur peut la lire, avec la
// raison d'accès correspondante.
//
// L'absence de filtre sur mw.deleted_at est voulue, et il faut le dire parce
// que source_rows en porte un juste à côté sur m.deleted_at: l'asymétrie a
// l'air d'un oubli et n'en est pas. Une source hors fenêtre qui a été
// supprimée continue donc de retenir le fait, définitivement, puisque rien ne
// retire jamais de ligne de graph_relation_sources. C'est le bon choix: le
// littéral que le modèle a levé de ce message survit dans graph_relations
// après sa suppression, donc le refuser à un demandeur qui n'a jamais eu accès
// qu'à la fenêtre octroyée est exactement ce que ce prédicat existe pour
// faire. Ajouter le filtre rendrait le fait au bout de la suppression du
// message, c'est-à-dire au moment où il devient le plus difficile à justifier.
//
// Le prix, à dire explicitement parce qu'il n'est pas évident: le prédicat
// s'évalue ligne par ligne, donc sur une seule unité à la fois. Un demandeur
// qui détiendrait deux octrois d'ACL dont les fenêtres couvrent, à elles deux,
// toutes les sources d'un fait ne l'obtient quand même pas: aucune des deux
// lignes ne satisfait « toutes les sources dans MA fenêtre ». C'est assumé.
// L'union des fenêtres serait plus généreuse et resterait correcte, mais elle
// demanderait de raisonner sur l'ensemble des octrois du demandeur plutôt que
// sur l'unité de la ligne, et la règle cesserait d'être vérifiable en lisant
// une ligne.
//
// Alias attendus: f pour la CTE facts, m pour le message source de la ligne
// courante, mu pour l'unité résolue sur ce message, r pour readable. Les
// mêmes que ceux d'accessReasonExpr et de ConversationGuardJoinOnMessages,
// puisque ce prédicat se lit dans le même WHERE.
const ExplicitACLSourceWindowPredicate = `
	AND (
		r.conversation_id IS NOT NULL
		OR NOT EXISTS (
			SELECT 1
			FROM graph_relation_sources gsw
			JOIN messages mw ON mw.message_id = gsw.message_id
			WHERE gsw.relation_id = f.relation_id
			  AND (
				mw.conversation_id <> m.conversation_id
				OR mw.sequence_number < mu.start_sequence
				OR mw.sequence_number > mu.end_sequence
			  )
		)
	)`
