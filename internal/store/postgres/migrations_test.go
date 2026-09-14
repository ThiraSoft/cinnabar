package postgres

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/pgvector/pgvector-go"

	"github.com/ThiraSoft/cinnabar/internal/config"
)

func TestMigrateCreatesSchema(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	want := []string{
		"conversations", "conversation_participants", "messages",
		"memory_units", "memory_unit_acl", "graph_entities",
		"graph_relations", "graph_relation_sources", "jobs", "api_clients",
	}
	for _, table := range want {
		var exists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)`, table).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("table %s absente", table)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// newTestPool a déjà migré une fois. Un second passage ne doit rien casser
	// ni rejouer, sinon un redémarrage du service échouerait.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("schema_migrations = %d lignes, want 6", n)
	}

	// Vérifie explicitement que les fichiers de migration sont enregistrés
	// par leur nom, pas seulement leur nombre: c'est la première fois que
	// le runner applique plusieurs migrations dans l'ordre, rien d'autre ne
	// l'exerçait avant l'ajout de 002_unique_client_label.sql.
	rows, err := pool.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Liste tenue à la main, et c'est voulu: ajouter une migration doit
	// être un geste délibéré qui passe par ici, plutôt qu'un fichier qui
	// s'applique en silence parce que le test se contentait de compter ce
	// que le paquet embarque.
	want := []string{
		"001_init.sql",
		"002_unique_client_label.sql",
		"003_jobs_running_index.sql",
		"004_graph_entities_workspace_index.sql",
		"005_graph_relations_embedding.sql",
		"006_metadata_filter.sql",
	}
	if len(names) != len(want) {
		t.Fatalf("schema_migrations noms = %v, want %v", names, want)
	}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("schema_migrations noms = %v, want %v", names, want)
			break
		}
	}
}

func TestGeneratedTsvIsPopulated(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id)
		VALUES ('c1', 'ws1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO messages
			(message_id, conversation_id, workspace_id, sequence_number,
			 author_key, role, content, created_at)
		VALUES (gen_random_uuid(), 'c1', 'ws1', 1, 'user:paul', 'user',
			'Mes tomates étaient encore vertes', now())`); err != nil {
		t.Fatal(err)
	}

	var n int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM messages
		WHERE tsv @@ websearch_to_tsquery('french', 'tomates')`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("recherche plein texte = %d, want 1", n)
	}
}

func TestRelationRequiresExactlyOneTarget(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_entities (entity_id, workspace_id, entity_type, display_name)
		VALUES ('11111111-1111-1111-1111-111111111111', 'ws1', 'person', 'Paul')`); err != nil {
		t.Fatal(err)
	}
	// Ni cible entité ni littéral: le CHECK doit refuser.
	_, err := pool.Exec(ctx, `
		INSERT INTO graph_relations
			(relation_id, workspace_id, source_entity_id, relation_type,
			 observed_at, scope, dedup_key)
		VALUES (gen_random_uuid(), 'ws1',
			'11111111-1111-1111-1111-111111111111', 'cultivates',
			now(), 'participants', 'k1')`)
	if err == nil {
		t.Fatal("une relation sans cible doit être refusée par le CHECK")
	}
}

// TestPoolCanWriteVectorsOnFreshDatabase vérifie qu'un vecteur à 768
// dimensions peut être écrit puis relu correctement, valeur par valeur, sur
// une base qui vient tout juste d'être migrée.
//
// MaxConns: 1 est délibéré: avec une seule connexion possible dans le pool,
// le test est déterministe plutôt que dépendant de la chance du pool. Dans
// le code actuel (Reset() en place dans Migrate), cette connexion unique est
// rouverte après la migration et enregistre donc normalement le type
// vector; c'est seulement dans l'hypothèse contraire, sans Reset(), qu'elle
// serait celle que Open() a créée pour son Ping, avant que la migration
// n'existe.
//
// Ce test ne prouve PAS que ce Reset() est nécessaire à cette écriture
// précise. pgvector.Vector implémente fmt.Stringer et sql.Scanner, deux
// interfaces vers lesquelles pgx sait retomber quand aucun codec n'est
// enregistré pour l'OID vector: l'écriture réussirait de toute façon par ce
// chemin de repli, avec ou sans enregistrement du type. Reset() est
// conservé pour l'encodage binaire et en défense en profondeur, pas parce
// que ce test le rendrait nécessaire.
func TestPoolCanWriteVectorsOnFreshDatabase(t *testing.T) {
	pool := newTestPoolWithConns(t, 1)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations (conversation_id, workspace_id)
		VALUES ('c1', 'ws1')`); err != nil {
		t.Fatal(err)
	}
	var messageID string
	err := pool.QueryRow(ctx, `
		INSERT INTO messages
			(message_id, conversation_id, workspace_id, sequence_number,
			 author_key, role, content, created_at)
		VALUES (gen_random_uuid(), 'c1', 'ws1', 1, 'user:paul', 'user',
			'Mes tomates étaient encore vertes', now())
		RETURNING message_id`).Scan(&messageID)
	if err != nil {
		t.Fatal(err)
	}

	embedding := make([]float32, 768)
	for i := range embedding {
		embedding[i] = float32(i) / 768
	}
	vec := pgvector.NewVector(embedding)

	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_units
			(memory_unit_id, workspace_id, conversation_id, anchor_message_id,
			 start_sequence, end_sequence, embedding_text, embedding,
			 embedding_model, indexing_strategy, indexing_version, scope)
		VALUES (gen_random_uuid(), 'ws1', 'c1', $1,
			1, 1, 'Mes tomates étaient encore vertes', $2,
			'test-model', 'contextualized_message', 1, 'participants')`,
		messageID, vec); err != nil {
		t.Fatalf("insert memory_unit avec embedding: %v", err)
	}

	var got pgvector.Vector
	err = pool.QueryRow(ctx, `
		SELECT embedding FROM memory_units WHERE anchor_message_id = $1`,
		messageID).Scan(&got)
	if err != nil {
		t.Fatalf("lecture embedding: %v", err)
	}

	gotSlice := got.Slice()
	if len(gotSlice) != len(embedding) {
		t.Fatalf("dimension du vecteur lu = %d, want %d", len(gotSlice), len(embedding))
	}
	for i, want := range embedding {
		if gotSlice[i] != want {
			t.Fatalf("embedding relu corrompu à l'indice %d: got %v, want %v", i, gotSlice[i], want)
		}
	}
}

// TestMigrateConcurrentIsSafe couvre le cas de plusieurs processus qui
// migrent la même base au même moment (plusieurs réplicas au démarrage,
// typiquement): sans le verrou consultatif pris dans migrations.Apply,
// CREATE TABLE IF NOT EXISTS n'est pas à l'abri d'une course entre sessions
// concurrentes, et la vérification "migration déjà appliquée" tourne hors
// de la transaction qui l'applique réellement, si bien que deux processus
// peuvent tous les deux décider de jouer 001_init.sql.
func TestMigrateConcurrentIsSafe(t *testing.T) {
	dsn := newTestDatabaseDSN(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pool, err := Open(ctx, config.Database{DSN: dsn, MaxConns: 2})
			if err != nil {
				errs[i] = fmt.Errorf("open: %w", err)
				return
			}
			defer pool.Close()
			errs[i] = Migrate(ctx, pool)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Migrate a échoué: %v", i, err)
		}
	}

	pool, err := Open(ctx, config.Database{DSN: dsn, MaxConns: 1})
	if err != nil {
		t.Fatalf("open pool de vérification: %v", err)
	}
	defer pool.Close()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Errorf("schema_migrations = %d lignes, want 6", count)
	}
}
