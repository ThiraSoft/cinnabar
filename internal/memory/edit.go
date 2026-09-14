package memory

import (
	"errors"

	"github.com/google/uuid"
)

// ErrNotFound signale un message inexistant ou déjà supprimé.
var ErrNotFound = errors.New("not found")

// ErrWorkspaceMismatch signale que conversation_id désigne une conversation
// déjà rattachée à un autre workspace. conversation_id est un TEXT libre
// choisi par le client, indexé seul (sans workspace_id), et donc devinable
// ou réutilisable par accident ou par malveillance depuis un autre
// workspace: le contrôle de token de la couche HTTP ne peut pas l'attraper,
// puisque l'appelant utilise bien son propre workspace_id, c'est la
// conversation qui appartient à quelqu'un d'autre. Déclarée dans le domaine
// (pas dans internal/store/postgres) pour qu'internal/api puisse la
// reconnaître sans importer le stockage: c'est MessageRepo.Append, sur son
// implémentation PostgreSQL, qui la produit, et le handler HTTP qui la
// traduit en 403 plutôt qu'en 500.
var ErrWorkspaceMismatch = errors.New("conversation belongs to a different workspace")

// EditResult porte le message modifié ou supprimé ainsi que l'état de
// désactivation des unités d'indexation qui en découle. Le type est déclaré
// ici, en tâche 14, parce qu'api.Editor le référence dans sa signature dès
// cette tâche pour figer le constructeur du serveur HTTP une seule fois; les
// méthodes qui le produisent (EditMessage, DeleteMessage sur *Ingester)
// arrivent en tâche 15.
type EditResult struct {
	Message            Message
	DeactivatedAnchors []uuid.UUID
	GraphStatus        string
}
