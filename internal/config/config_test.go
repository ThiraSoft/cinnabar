package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExpandsEnvAndAppliesDefaults(t *testing.T) {
	t.Setenv("PG_DSN", "postgres://u:p@localhost:5433/cinnabar")
	t.Setenv("LLM_API_URL", "https://llm.example")

	path := writeTemp(t, `
database:
  dsn: "${PG_DSN}"
extraction:
  base_url: "${LLM_API_URL}/v1"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.DSN != "postgres://u:p@localhost:5433/cinnabar" {
		t.Errorf("dsn not expanded: %q", cfg.Database.DSN)
	}
	if cfg.Extraction.BaseURL != "https://llm.example/v1" {
		t.Errorf("base_url not expanded: %q", cfg.Extraction.BaseURL)
	}
	// Valeurs par défaut issues de la section 10 de la spec.
	if cfg.Retrieval.RRFK != 60 {
		t.Errorf("rrf_k default = %d, want 60", cfg.Retrieval.RRFK)
	}
	if cfg.Retrieval.FinalTopK != 5 {
		t.Errorf("final_top_k default = %d, want 5", cfg.Retrieval.FinalTopK)
	}
	if cfg.Indexing.MaxChars != 1600 {
		t.Errorf("max_chars default = %d, want 1600", cfg.Indexing.MaxChars)
	}
	if cfg.Embedding.Dimensions != 768 {
		t.Errorf("dimensions default = %d, want 768", cfg.Embedding.Dimensions)
	}
	if cfg.Embedding.QueryPrefix != "search_query: " {
		t.Errorf("query_prefix default = %q", cfg.Embedding.QueryPrefix)
	}
	if cfg.Service.ShutdownGrace != 30*time.Second {
		t.Errorf("shutdown_grace default = %v", cfg.Service.ShutdownGrace)
	}
	if cfg.Service.DebugSearch {
		t.Error("debug_search doit être false par défaut")
	}
	if cfg.Access.DefaultScope != "participants" {
		t.Errorf("default_scope = %q", cfg.Access.DefaultScope)
	}
	// graph.enabled doit être false par défaut: un config.yaml qui omet le
	// bloc graph (ou écrit avant que le plan graphe ne soit livré) ne doit
	// pas se retrouver à poser des jobs graph_extract/graph_reeval que rien
	// ne consomme.
	if cfg.Graph.Enabled {
		t.Error("graph.enabled doit être false par défaut")
	}
}

// TestLoadConfigExampleYAML charge le vrai config.example.yaml du dépôt
// avec exactement les variables d'environnement documentées par le README
// (quickstart), rien de plus: si l'une des deux dérive de l'autre sans que
// personne ne s'en aperçoive, ce test casse avant qu'un opérateur ne le
// découvre en suivant le README à la lettre.
func TestLoadConfigExampleYAML(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://cinnabar:cinnabar@localhost:5433/cinnabar?sslmode=disable")
	t.Setenv("LLM_API_URL", "http://localhost:9999")
	t.Setenv("LLM_API_KEY", "dummy")

	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("Load(config.example.yaml): %v", err)
	}

	if cfg.Graph.Enabled {
		t.Error("config.example.yaml: graph.enabled doit rester false tant que le plan graphe n'est pas livré")
	}
	// Les plafonds qui protègent un tenant d'un autre sur un service
	// partagé: exposés dans le fichier d'exemple, pas seulement dans les
	// défauts du code, pour qu'un opérateur sache qu'ils existent.
	if cfg.Service.MaxCandidateLimit != 500 {
		t.Errorf("max_candidate_limit = %d, want 500", cfg.Service.MaxCandidateLimit)
	}
	if cfg.Service.MaxResultLimit != 50 {
		t.Errorf("max_result_limit = %d, want 50", cfg.Service.MaxResultLimit)
	}
	if cfg.Service.MaxTokenBudget != 20000 {
		t.Errorf("max_token_budget = %d, want 20000", cfg.Service.MaxTokenBudget)
	}
}

func TestLoadMissingEnvVarIsAnError(t *testing.T) {
	os.Unsetenv("CINNABAR_NOPE")
	path := writeTemp(t, "database:\n  dsn: \"${CINNABAR_NOPE}\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("une variable d'environnement absente doit produire une erreur")
	}
}

func TestValidateRejectsUnknownScope(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\naccess:\n  default_scope: \"nope\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("un scope inconnu doit produire une erreur")
	}
}

// TestValidateRejectsReclaimAfterTooSmall épingle le trou que
// config.validate n'attrapait pas: rien ne vérifiait que
// jobs.reclaim_after laisse une marge suffisante au-dessus du timeout de
// traitement d'un job (5 minutes). Un reclaim_after de 1 minute, accepté en
// silence, ferait reclaimer tout job dont le handler dépasse une minute
// alors qu'il tourne encore, gonflant attempts sur un job sain jusqu'à le
// dead-lettrer sans qu'il n'ait jamais réellement échoué.
func TestValidateRejectsReclaimAfterTooSmall(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\njobs:\n  reclaim_after: 1m\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("un reclaim_after inférieur à deux fois le timeout du handler doit être rejeté")
	}
}

// TestValidateAcceptsReclaimAfterAtExactlyTwiceTheHandlerTimeout vérifie que
// la valeur par défaut (10 minutes, soit exactement deux fois les 5 minutes
// du timeout de handle) n'est pas elle-même rejetée: la borne est un
// minimum inclusif, pas exclusif.
func TestValidateAcceptsReclaimAfterAtExactlyTwiceTheHandlerTimeout(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\njobs:\n  reclaim_after: 10m\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("10m ne doit pas être rejeté, c'est exactement le minimum: %v", err)
	}
}

// TestConfigGraphDesactiveParDefaut reprend TestLoadExpandsEnvAndAppliesDefaults
// pour un fichier qui n'écrit aucun bloc graph du tout: un déploiement
// existant, écrit avant que le graphe n'existe, ne doit changer de
// comportement en rien.
func TestConfigGraphDesactiveParDefaut(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Graph.Enabled {
		t.Error("graph.enabled vaut vrai par défaut: un fichier sans bloc graph se mettrait à appeler un LLM")
	}
}

// TestConfigGraphExigeUnExtracteurQuandActive épingle la règle qui rend
// graph.enabled: true sans extraction.base_url un échec de démarrage plutôt
// qu'un service qui poserait des jobs graph_extract qu'aucun appel réseau
// ne pourrait jamais honorer.
func TestConfigGraphExigeUnExtracteurQuandActive(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\ngraph:\n  enabled: true\n")
	if _, err := Load(path); err == nil {
		t.Fatal("want une erreur: graph.enabled sans extraction.base_url ne peut pas marcher")
	}
}

// TestConfigGraphAcceptsEnabledWithBaseURL vérifie le contrepoint du test
// précédent: la même configuration, complétée par extraction.base_url,
// n'est pas rejetée pour autant.
func TestConfigGraphAcceptsEnabledWithBaseURL(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\nextraction:\n  base_url: \"http://llm.example\"\ngraph:\n  enabled: true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("graph.enabled avec extraction.base_url ne doit pas être rejeté: %v", err)
	}
	if !cfg.Graph.Enabled {
		t.Error("graph.enabled = false, want true")
	}
}

// TestConfigGraphBorneMaxHops: 0 ou négatif remonte à 1, au-delà de 3 est
// refusé. Une CTE récursive sur un graphe dense explose au-delà, et la
// section 8.2 de la spec ne demande qu'un ou deux sauts.
func TestConfigGraphBorneMaxHops(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\ngraph:\n  enabled: false\n  max_hops: 0\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Graph.MaxHops != 1 {
		t.Errorf("max_hops = %d, want 1", cfg.Graph.MaxHops)
	}

	path = writeTemp(t, "database:\n  dsn: \"x\"\ngraph:\n  enabled: false\n  max_hops: -3\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Graph.MaxHops != 1 {
		t.Errorf("max_hops négatif = %d, want 1", cfg.Graph.MaxHops)
	}

	path = writeTemp(t, "database:\n  dsn: \"x\"\ngraph:\n  enabled: false\n  max_hops: 9\n")
	if _, err := Load(path); err == nil {
		t.Error("want une erreur sur max_hops = 9")
	}

	// La borne haute est inclusive: 3 doit passer.
	path = writeTemp(t, "database:\n  dsn: \"x\"\ngraph:\n  enabled: false\n  max_hops: 3\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("max_hops = 3 ne doit pas être rejeté: %v", err)
	}
	if cfg.Graph.MaxHops != 3 {
		t.Errorf("max_hops = %d, want 3", cfg.Graph.MaxHops)
	}
}

// TestConfigGraphRejectsNegativeContextMessages: un context_messages
// négatif n'a pas de sens (on ne peut pas fournir un nombre négatif de
// messages de contexte à l'extracteur) et doit être refusé au démarrage
// plutôt que de produire un slice négatif plus loin dans le pipeline.
func TestConfigGraphRejectsNegativeContextMessages(t *testing.T) {
	path := writeTemp(t, "database:\n  dsn: \"x\"\ngraph:\n  enabled: false\n  context_messages: -1\n")
	if _, err := Load(path); err == nil {
		t.Error("want une erreur sur context_messages négatif")
	}
}

// TestConfigEmbeddingGolem: le mode golem exige un model_path et un device
// connu, et un fournisseur inconnu est refusé au démarrage.
func TestConfigEmbeddingGolem(t *testing.T) {
	cases := []struct {
		name, yaml string
		ok         bool
	}{
		{"http par défaut", "", true},
		{"golem sans chemin", "  provider: golem\n", false},
		{"golem cpu", "  provider: golem\n  model_path: \"/m.gguf\"\n", true},
		{"golem cpu explicite", "  provider: golem\n  model_path: \"/m.gguf\"\n  device: cpu\n", true},
		{"golem device inconnu", "  provider: golem\n  model_path: \"/m.gguf\"\n  device: cuda\n", false},
		{"fournisseur inconnu", "  provider: openai\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTemp(t, "database:\n  dsn: \"x\"\nembedding:\n  dimensions: 768\n"+c.yaml)
			_, err := Load(path)
			if c.ok && err != nil {
				t.Fatalf("rejeté à tort: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("want une erreur")
			}
		})
	}
}
