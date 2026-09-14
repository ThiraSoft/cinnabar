package memory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// trackingMessageRepo simule les deux méthodes combinées atomiques
// (EditAndDeactivate, SoftDeleteAndDeactivate): EditMessage et DeleteMessage
// ne passent plus par UnitRepo.DeactivateCovering séparément, donc les
// anchors désactivées sont désormais rendues directement par ce repo, pas
// par un UnitRepo de test. fakeUnitRepo (zéro-valeur) suffit comme UnitRepo
// dans ces tests: son DeactivateCovering n'est plus appelé par ces chemins.
type trackingMessageRepo struct {
	fakeMessageRepo
	edited  string
	deleted bool
	out     Message
	// anchors simule ce que la méthode combinée désactiverait: les
	// identifiants de message ancre des unités couvrant la séquence
	// éditée ou supprimée. Peut contenir out.MessageID lui-même (l'unité
	// ancrée sur le message qu'on touche) ainsi que des voisins vivants.
	anchors []uuid.UUID
}

func (t *trackingMessageRepo) EditAndDeactivate(_ context.Context, _ uuid.UUID,
	c string) (Message, []uuid.UUID, error) {
	t.edited = c
	return t.out, t.anchors, nil
}

func (t *trackingMessageRepo) SoftDeleteAndDeactivate(context.Context, uuid.UUID) (Message, []uuid.UUID, error) {
	t.deleted = true
	now := time.Now()
	m := t.out
	m.DeletedAt = &now
	return m, t.anchors, nil
}

// ByID rend le message suivi (out) comme mort dès qu'on le redemande par son
// propre identifiant, quel que soit l'ordre d'appel: DeleteMessage recharge
// chaque ancre pour savoir si elle est encore vivante avant de poser un job
// de reconstruction, et l'ancre du message qu'on vient de supprimer ne doit
// jamais en recevoir un. Toute autre identifiant est traité comme un
// voisin vivant (le comportement par défaut de fakeMessageRepo.ByID).
func (t *trackingMessageRepo) ByID(ctx context.Context, id uuid.UUID) (Message, error) {
	if id == t.out.MessageID {
		now := time.Now()
		m := t.out
		m.DeletedAt = &now
		return m, nil
	}
	return t.fakeMessageRepo.ByID(ctx, id)
}

func TestEditMessageInvalidatesAndRebuilds(t *testing.T) {
	target := msg(7, "user:paul", "user", "nouveau contenu")
	anchorA, anchorB := uuid.New(), uuid.New()

	msgs := &trackingMessageRepo{out: target, anchors: []uuid.UUID{anchorA, anchorB}}
	units := &fakeUnitRepo{}
	q := &fakeQueue{}
	ing := newIngesterWith(msgs, units, q)

	res, err := ing.EditMessage(context.Background(), target.MessageID, "nouveau contenu")
	if err != nil {
		t.Fatal(err)
	}
	if msgs.edited != "nouveau contenu" {
		t.Errorf("contenu transmis = %q", msgs.edited)
	}
	// L'invalidation porte sur toutes les unités qui couvrent la séquence, pas
	// seulement celles ancrées sur le message.
	if len(res.DeactivatedAnchors) != 2 {
		t.Errorf("%d ancres à reconstruire, want 2", len(res.DeactivatedAnchors))
	}
	// Un job de reconstruction par ancre touchée, plus la réévaluation graphe.
	embeds, reevals := 0, 0
	for _, j := range q.enqueued {
		switch j {
		case "embed":
			embeds++
		case "graph_reeval":
			reevals++
		}
	}
	if embeds != 2 {
		t.Errorf("%d jobs embed, want 2 (un par ancre)", embeds)
	}
	if reevals != 1 {
		t.Errorf("%d jobs graph_reeval, want 1", reevals)
	}
}

func TestDeleteMessageDeactivatesImmediately(t *testing.T) {
	target := msg(7, "user:paul", "user", "à supprimer")
	// La seule unité couvrante ici est celle ancrée sur le message supprimé
	// lui-même (le cas le plus courant: un message isolé, sans voisin dont
	// le contexte le citait). Sa reconstruction ne servirait à rien.
	msgs := &trackingMessageRepo{out: target, anchors: []uuid.UUID{target.MessageID}}
	units := &fakeUnitRepo{}
	q := &fakeQueue{}
	ing := newIngesterWith(msgs, units, q)

	res, err := ing.DeleteMessage(context.Background(), target.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if !msgs.deleted {
		t.Error("le message doit être marqué supprimé")
	}
	if len(res.DeactivatedAnchors) != 1 {
		t.Error("les unités couvrantes doivent être désactivées immédiatement")
	}
	if res.Message.DeletedAt == nil {
		t.Error("le message rendu doit porter sa date de suppression")
	}
	// Aucune ancre vivante ici: pas de job de reconstruction, seulement la
	// réévaluation graphe.
	for _, j := range q.enqueued {
		if j == "embed" {
			t.Error("l'ancre du message supprimé lui-même ne doit jamais être reconstruite")
		}
	}
	if len(q.enqueued) != 1 || q.enqueued[0] != "graph_reeval" {
		t.Errorf("jobs = %v, want [graph_reeval]", q.enqueued)
	}
}

// TestDeleteMessageRebuildsLiveNeighboursNotDeletedAnchor couvre le correctif
// signalé par la revue: la désactivation est large (toute unité qui couvre
// la séquence, pas seulement celle ancrée dessus), donc supprimer un message
// désactive aussi les unités de ses voisins vivants dont le contexte le
// citait. Sans reconstruction, ces voisins disparaîtraient du dense pour
// toujours puisqu'une suppression ne pose par ailleurs aucun job de
// rattrapage général.
func TestDeleteMessageRebuildsLiveNeighboursNotDeletedAnchor(t *testing.T) {
	target := msg(7, "user:paul", "user", "à supprimer")
	neighbourA, neighbourB := uuid.New(), uuid.New()
	msgs := &trackingMessageRepo{out: target,
		anchors: []uuid.UUID{target.MessageID, neighbourA, neighbourB}}
	units := &fakeUnitRepo{}
	q := &fakeQueue{}
	ing := newIngesterWith(msgs, units, q)

	if _, err := ing.DeleteMessage(context.Background(), target.MessageID); err != nil {
		t.Fatal(err)
	}

	var rebuilt []uuid.UUID
	for i, j := range q.enqueued {
		if j != "embed" {
			continue
		}
		payload, ok := q.payloads[i].(map[string]any)
		if !ok {
			t.Fatalf("payload embed inattendu: %#v", q.payloads[i])
		}
		id, ok := payload["message_id"].(uuid.UUID)
		if !ok {
			t.Fatalf("message_id absent ou du mauvais type: %#v", payload["message_id"])
		}
		rebuilt = append(rebuilt, id)
	}

	if len(rebuilt) != 2 {
		t.Fatalf("%d jobs embed, want 2 (un par voisin vivant): %v", len(rebuilt), rebuilt)
	}
	for _, id := range rebuilt {
		if id == target.MessageID {
			t.Error("le message supprimé lui-même ne doit jamais être reconstruit")
		}
	}
	want := map[uuid.UUID]bool{neighbourA: true, neighbourB: true}
	for _, id := range rebuilt {
		if !want[id] {
			t.Errorf("ancre reconstruite inattendue: %s", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("voisins vivants jamais reconstruits: %v", want)
	}
}
