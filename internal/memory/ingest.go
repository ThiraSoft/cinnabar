package memory

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// IngestResult porte ce que l'appelant a besoin de savoir sur une écriture:
// le message tel que stocké, si l'appel était un rejeu, et l'état
// d'indexation vectorielle et graphe qui en a résulté.
type IngestResult struct {
	Message       Message
	Replayed      bool
	Status        string
	UnitIDs       []uuid.UUID
	GraphStatus   string
	IndexingError string

	// EmbedScheduled dit qu'un job embed est bien posé pour ce message.
	// Sans lui, un statut "stored" ne distingue pas une indexation
	// seulement différée d'une indexation définitivement perdue: les deux
	// répondent exactement la même chose. Faux sur un rejeu (l'unité existe
	// déjà), faux quand le rôle du message n'est pas indexé, faux aussi en
	// mode searchable réussi, où l'unité est écrite en ligne et où il n'y a
	// donc rien à rattraper.
	EmbedScheduled bool
}

// Ingester orchestre une écriture de bout en bout: persistance du message,
// puis indexation vectorielle et mise en file du graphe selon le mode de
// cohérence demandé.
type Ingester struct {
	cfg   *config.Config
	msgs  MessageRepo
	units UnitRepo
	emb   Embedder
	jobs  JobQueue
}

func NewIngester(cfg *config.Config, msgs MessageRepo, units UnitRepo,
	emb Embedder, jobs JobQueue) *Ingester {
	return &Ingester{cfg: cfg, msgs: msgs, units: units, emb: emb, jobs: jobs}
}

// Ingest enregistre un message puis, selon le mode de cohérence, construit son
// unité vectorielle immédiatement ou pose un job.
//
// Un échec d'embedding en mode searchable ne fait pas échouer l'appel: le
// message est déjà durable, donc rendre une erreur mentirait à l'appelant. Le
// statut dégrade en "stored" et un job de rattrapage est posé.
func (i *Ingester) Ingest(ctx context.Context, in AppendInput,
	consistency string) (IngestResult, error) {

	if in.DefaultScope == "" {
		in.DefaultScope = i.cfg.Access.DefaultScope
	}
	if consistency == "" {
		consistency = i.cfg.Service.ConsistencyDefault
	}

	// Les jobs sont décrits avant l'écriture et posés par le repo dans la
	// transaction d'Append (section 6.1 de la spec, étape 6), pas après le
	// commit sur une autre connexion: une panne entre les deux laissait
	// sinon un message durable que plus rien ne viendrait jamais indexer.
	//
	// Le rôle suffit à décider si une unité sera construite: c'est le seul
	// critère par lequel BuildUnit peut refuser d'indexer (ShouldIndex), et
	// il est connu avant l'écriture. En mode searchable, l'unité est écrite
	// en ligne juste après: aucun job embed n'est posé d'avance, seul un
	// échec de l'embedder en pose un, forcément après le commit.
	embedDeferred := ShouldIndex(i.cfg.Indexing, in.Role) && consistency != "searchable"
	var pending []AppendJob
	if embedDeferred {
		pending = append(pending, AppendJob{Type: "embed"})
	}
	if i.cfg.Graph.Enabled {
		pending = append(pending, AppendJob{Type: "graph_extract"})
	}

	appended, err := i.msgs.Append(ctx, in, i.cfg.Indexing.PreviousMessages, pending)
	if err != nil {
		return IngestResult{}, err
	}

	res := IngestResult{
		Message:  appended.Message,
		Replayed: appended.Replayed,
		Status:   "stored",
	}

	if i.cfg.Graph.Enabled {
		res.GraphStatus = "pending"
	} else {
		res.GraphStatus = "disabled"
	}

	// Un rejeu ne réindexe rien: l'unité existe déjà et son identifiant est
	// déterministe.
	if appended.Replayed {
		if res.GraphStatus == "pending" {
			res.GraphStatus = "already_requested"
		}
		return res, nil
	}

	// Le job embed du mode différé est déjà posé, dans la transaction qui a
	// écrit le message: le client peut donc distinguer une indexation
	// différée d'une indexation perdue.
	res.EmbedScheduled = embedDeferred

	unit, indexable := BuildUnit(i.cfg.Indexing, i.emb.Model(), appended.Scope,
		appended.Participants, appended.Message, appended.Previous)

	if indexable && consistency == "searchable" {
		if err := i.indexNow(ctx, unit); err != nil {
			res.IndexingError = err.Error()
			// Seul rattrapage qui ne peut pas entrer dans la transaction:
			// l'échec n'est connu qu'une fois le message committé. Un
			// enqueue qui échoue à son tour laisse EmbedScheduled à faux,
			// ce qui est précisément le signal que l'appelant attend.
			res.EmbedScheduled = i.enqueueEmbed(ctx, appended)
			slog.WarnContext(ctx, "inline indexing failed, deferred",
				"conversation_id", appended.Message.ConversationID,
				"error", err)
		} else {
			res.Status = "searchable"
			res.UnitIDs = []uuid.UUID{unit.MemoryUnitID}
		}
	}

	return res, nil
}

// MessageOwner rend le workspace et l'auteur d'un message sans en charger le
// contenu, pour que la couche HTTP puisse vérifier l'appartenance au
// workspace et le périmètre d'identité avant toute modification, plutôt
// qu'après coup: un refus qui arriverait après une écriture aurait déjà
// laissé une trace dans un workspace ou sous une identité qui n'est pas
// celle de l'appelant.
func (i *Ingester) MessageOwner(ctx context.Context, messageID uuid.UUID) (string, string, error) {
	return i.msgs.MessageOwner(ctx, messageID)
}

// EditMessage met à jour le contenu et désactive les unités qui couvrent ce
// message en une seule opération atomique (EditAndDeactivate), pour la même
// raison que DeleteMessage utilise SoftDeleteAndDeactivate: une panne entre
// les deux effets laisserait une unité active citer un texte qui ne
// correspond plus au message, une contradiction du dossier puisque le
// message affiche déjà le nouveau texte.
//
// L'invalidation ne se limite pas aux unités ancrées sur le message: une unité
// voisine qui le citait en contexte porterait un texte devenu faux. La pose
// des jobs de reconstruction reste hors transaction: un échec d'enqueue est
// déjà journalisé plus bas et n'a pas à annuler une édition par ailleurs
// légitime.
func (i *Ingester) EditMessage(ctx context.Context, messageID uuid.UUID,
	content string) (EditResult, error) {

	updated, anchors, err := i.msgs.EditAndDeactivate(ctx, messageID, content)
	if err != nil {
		return EditResult{}, err
	}

	for _, anchor := range anchors {
		if err := i.jobs.Enqueue(ctx, "embed",
			updated.WorkspaceID, updated.ConversationID,
			map[string]any{"message_id": anchor}); err != nil {
			slog.ErrorContext(ctx, "enqueue rebuild failed",
				"conversation_id", updated.ConversationID, "error", err)
		}
	}

	res := EditResult{Message: updated, DeactivatedAnchors: anchors,
		GraphStatus: "disabled"}
	if i.cfg.Graph.Enabled {
		res.GraphStatus = "pending"
		if err := i.jobs.Enqueue(ctx, "graph_reeval",
			updated.WorkspaceID, updated.ConversationID,
			map[string]any{"message_id": updated.MessageID}); err != nil {
			res.GraphStatus = "enqueue_failed"
		}
	}
	return res, nil
}

// DeleteMessage fait un soft delete et désactive les unités concernées dans
// une seule opération atomique (SoftDeleteAndDeactivate), ce qui satisfait le
// critère 6: le contenu ne ressort plus dès la requête suivante, sans
// attendre un worker, et sans fenêtre où une panne laisserait des unités
// actives citer un message qui n'existe plus.
//
// La désactivation est volontairement large: toute unité qui couvre la
// séquence du message supprimé, pas seulement celle ancrée dessus. Une unité
// voisine, ancrée sur un message resté vivant, qui citait le message
// supprimé en contexte peut donc se retrouver désactivée elle aussi. Sans
// reconstruction, ce voisin vivant disparaîtrait du dense pour toujours,
// puisqu'une suppression ne pose par ailleurs aucun job de rattrapage
// général. On pose donc un job embed pour chaque ancre désactivée qui est
// encore vivante — jamais pour le message qu'on vient de supprimer
// lui-même (ni pour une ancre déjà morte par ailleurs): EmbedHandler
// refuserait de toute façon de le reconstruire, et le poser quand même
// gonflerait pour rien les compteurs de lettres mortes du worker.
func (i *Ingester) DeleteMessage(ctx context.Context,
	messageID uuid.UUID) (EditResult, error) {

	deleted, anchors, err := i.msgs.SoftDeleteAndDeactivate(ctx, messageID)
	if err != nil {
		return EditResult{}, err
	}

	for _, anchor := range anchors {
		anchorMsg, err := i.msgs.ByID(ctx, anchor)
		if err != nil {
			slog.ErrorContext(ctx, "load anchor for rebuild failed",
				"conversation_id", deleted.ConversationID, "error", err)
			continue
		}
		if anchorMsg.DeletedAt != nil {
			continue
		}
		if err := i.jobs.Enqueue(ctx, "embed",
			deleted.WorkspaceID, deleted.ConversationID,
			map[string]any{"message_id": anchor}); err != nil {
			slog.ErrorContext(ctx, "enqueue rebuild failed",
				"conversation_id", deleted.ConversationID, "error", err)
		}
	}

	res := EditResult{Message: deleted, DeactivatedAnchors: anchors,
		GraphStatus: "disabled"}
	if i.cfg.Graph.Enabled {
		res.GraphStatus = "pending"
		if err := i.jobs.Enqueue(ctx, "graph_reeval",
			deleted.WorkspaceID, deleted.ConversationID,
			map[string]any{"message_id": deleted.MessageID}); err != nil {
			res.GraphStatus = "enqueue_failed"
		}
	}
	return res, nil
}

func (i *Ingester) indexNow(ctx context.Context, unit Unit) error {
	vecs, err := i.emb.Embed(ctx, []string{unit.EmbeddingText})
	if err != nil {
		return err
	}
	return i.units.Upsert(ctx, unit, vecs[0])
}

// enqueueEmbed pose un job embed hors transaction et dit s'il a été accepté.
// N'est employée que sur le rattrapage d'un échec d'indexation en ligne, le
// seul cas où le besoin d'un job n'est connu qu'après le commit du message:
// tous les autres jobs entrent dans la transaction d'Append.
func (i *Ingester) enqueueEmbed(ctx context.Context, appended AppendResult) bool {
	err := i.jobs.Enqueue(ctx, "embed",
		appended.Message.WorkspaceID, appended.Message.ConversationID,
		map[string]any{"message_id": appended.Message.MessageID})
	if err != nil {
		slog.ErrorContext(ctx, "enqueue embed failed",
			"conversation_id", appended.Message.ConversationID, "error", err)
		return false
	}
	return true
}
