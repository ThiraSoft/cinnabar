package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/memory"
)

func rerankServer(t *testing.T, contenu string, code int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != 0 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"type":"server_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + contenu + `}}]}`))
	}))
}

func trois() []memory.RerankCandidate {
	return []memory.RerankCandidate{{Text: "a"}, {Text: "b"}, {Text: "c"}}
}

func TestRerankRendLOrdreDuModele(t *testing.T) {
	srv := rerankServer(t, `"{\"ordre\":[2,0,1]}"`, 0)
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	got, err := r.Rerank(context.Background(), "q", trois())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 0 || got[2] != 1 {
		t.Errorf("ordre = %v, want [2 0 1]", got)
	}
}

// TestRerankReintroduitLesPositionsOmises: un modèle bavard qui ne rend que
// ses deux préférés ne doit pas faire disparaître le troisième extrait. Ce
// qu'il omet revient à la fin, dans l'ordre de la fusion.
func TestRerankReintroduitLesPositionsOmises(t *testing.T) {
	srv := rerankServer(t, `"{\"ordre\":[2]}"`, 0)
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	got, err := r.Rerank(context.Background(), "q", trois())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ordre = %v, want trois positions", got)
	}
	if got[0] != 2 || got[1] != 0 || got[2] != 1 {
		t.Errorf("ordre = %v, want [2 0 1]: le préféré devant, le reste "+
			"dans l'ordre de la fusion", got)
	}
}

// TestRerankEcarteLesPositionsInventeesEtRepetees: un numéro hors bornes
// ferait sortir l'appelant du tableau, un numéro répété dupliquerait un
// extrait. Aucun des deux ne doit être cru.
func TestRerankEcarteLesPositionsInventeesEtRepetees(t *testing.T) {
	srv := rerankServer(t, `"{\"ordre\":[1,1,99,-3,0]}"`, 0)
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	got, err := r.Rerank(context.Background(), "q", trois())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ordre = %v, want exactement trois positions distinctes", got)
	}
	vu := map[int]bool{}
	for _, i := range got {
		if i < 0 || i >= 3 {
			t.Errorf("position hors bornes dans %v", got)
		}
		if vu[i] {
			t.Errorf("position répétée dans %v", got)
		}
		vu[i] = true
	}
	if got[0] != 1 {
		t.Errorf("ordre = %v, want la première position valide en tête", got)
	}
}

func TestRerankSurUneReponseIllisibleEchoue(t *testing.T) {
	srv := rerankServer(t, `"je ne sais pas"`, 0)
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	if _, err := r.Rerank(context.Background(), "q", trois()); err == nil {
		t.Fatal("want une erreur: l'appelant doit retomber sur l'ordre de la fusion")
	}
}

// L'erreur ne doit pas recopier le contenu des extraits ni la réponse du
// modèle: elle remonte jusqu'au log du service.
func TestRerankNeRecopiePasLeContenu(t *testing.T) {
	const secret = "les tomates de Paul sont vertes"
	srv := rerankServer(t, `"`+secret+`"`, 0)
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	_, err := r.Rerank(context.Background(), "q",
		[]memory.RerankCandidate{{Text: secret}})
	if err == nil {
		t.Fatal("want une erreur")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("l'erreur recopie le contenu: %v", err)
	}
}

func TestRerankPropageLEchecDuTransport(t *testing.T) {
	srv := rerankServer(t, "", http.StatusInternalServerError)
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	_, err := r.Rerank(context.Background(), "q", trois())
	if err == nil {
		t.Fatal("want une erreur sur un 500")
	}
}

func TestRerankSansCandidatNAppellePasLeModele(t *testing.T) {
	appele := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appele = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewReranker(config.Rerank{BaseURL: srv.URL, Model: "m",
		Timeout: 5 * time.Second, Pool: 10})
	got, err := r.Rerank(context.Background(), "q", nil)
	if err != nil || got != nil {
		t.Errorf("got %v, %v; want nil, nil", got, err)
	}
	if appele {
		t.Error("le modèle a été appelé pour zéro candidat")
	}
}
