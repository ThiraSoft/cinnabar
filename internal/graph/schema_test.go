package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

// TestSingleValuedDefaultsExistentDansLePrompt garde l'accord entre deux
// fichiers que rien ne relie autrement: la liste des types de relations à
// valeur unique, dans la configuration, et le vocabulaire de prédicats que le
// prompt impose au modèle.
//
// Ce test existe parce que le désaccord est arrivé et qu'il est resté invisible
// jusqu'à une évaluation contre le vrai extracteur. Le défaut était
// has_observed_state, un identifiant anglais, pendant que le prompt était en
// français et que le modèle produisait a_pour_etat. Conséquence: tout le
// chaînage temporel, la fermeture des fenêtres de validité, le recalcul des
// chaînes et les deux correctifs de concurrence qu'ils ont coûté ne se
// déclenchaient jamais. Aucune erreur, aucun test rouge, juste une
// fonctionnalité morte.
//
// Le test ne vérifie pas que le modèle obéit, ce qu'aucun test ne peut faire.
// Il vérifie qu'on lui a au moins demandé le bon mot.
func TestSingleValuedDefaultsExistentDansLePrompt(t *testing.T) {
	// La configuration est chargée depuis un fichier minimal plutôt que
	// depuis une constante recopiée ici: c'est le défaut réellement servi
	// à un déploiement qui omet le bloc graph qu'on veut garder, pas une
	// copie qui pourrait dériver de lui.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("service:\n  listen: \":8080\"\ndatabase:\n  dsn: \"postgres://x/y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Graph.SingleValuedRelations) == 0 {
		t.Fatal("aucun type à valeur unique par défaut: le test ne prouve rien")
	}
	for _, rt := range cfg.Graph.SingleValuedRelations {
		if !strings.Contains(systemPrompt, rt) {
			t.Errorf("le type à valeur unique %q n'apparaît pas dans le "+
				"vocabulaire du prompt: le chaînage temporel ne se déclenchera "+
				"jamais, puisque le modèle ne se voit jamais demander ce "+
				"prédicat", rt)
		}
	}
}
