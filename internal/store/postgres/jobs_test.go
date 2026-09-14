package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestJobRepoEnqueueAndClaim(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1",
		map[string]any{"message_id": "m1"}); err != nil {
		t.Fatal(err)
	}

	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}
	if j.JobType != "embed" || j.ConversationID != "conv_1" {
		t.Errorf("job = %+v", j)
	}

	// Un job réclamé ne doit pas être réclamé une seconde fois.
	again, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Error("un job en cours ne doit pas être réclamé deux fois")
	}
}

func TestJobRepoClaimOnEmptyQueue(t *testing.T) {
	pool := newTestPool(t)
	repo := NewJobRepo(pool)
	j, err := repo.Claim(context.Background())
	if err != nil {
		t.Fatalf("une file vide n'est pas une erreur: %v", err)
	}
	if j != nil {
		t.Error("une file vide doit rendre nil")
	}
}

func TestJobRepoFailRetriesThenDies(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}

	const retryLimit = 3
	for attempt := 1; attempt <= retryLimit; attempt++ {
		j, err := repo.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			t.Fatalf("tentative %d: aucun job réclamé", attempt)
		}
		if err := repo.Fail(ctx, j.JobID, errors.New("boom"), retryLimit); err != nil {
			t.Fatal(err)
		}
		// Le backoff repousse run_after, donc on le remet à maintenant pour
		// tester la logique de retry sans attendre.
		if _, err := pool.Exec(ctx,
			`UPDATE jobs SET run_after = now() WHERE job_id = $1`, j.JobID); err != nil {
			t.Fatal(err)
		}
	}

	var status string
	var attempts int
	var lastError *string
	err := pool.QueryRow(ctx,
		`SELECT status, attempts, last_error FROM jobs`).Scan(&status, &attempts, &lastError)
	if err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Errorf("status = %q, want dead après %d échecs", status, retryLimit)
	}
	if attempts != retryLimit {
		t.Errorf("attempts = %d, want %d", attempts, retryLimit)
	}
	if lastError == nil || *lastError == "" {
		t.Error("last_error doit être renseigné pour la file de lettres mortes")
	}

	// Un job mort ne repart pas.
	if j, err := repo.Claim(ctx); err != nil || j != nil {
		t.Errorf("un job mort ne doit plus être réclamé: %v, %v", j, err)
	}
}

// TestJobRepoDepth couvre aussi bien pending que dead: le compte dead est
// ce qu'un opérateur regarde en premier après un incident, et il doit
// rester disponible séparément du pending plutôt que fondu avec lui.
func TestJobRepoDepth(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	for i := 0; i < 3; i++ {
		if err := repo.Enqueue(ctx, "embed", "ws1", "c", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Enqueue(ctx, "graph_extract", "ws1", "c", map[string]any{}); err != nil {
		t.Fatal(err)
	}

	// Un embed mort de plus, pour vérifier que Depth le compte à part.
	if err := repo.Enqueue(ctx, "embed", "ws1", "c", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	dead, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dead == nil {
		t.Fatal("Claim doit rendre le job posé")
	}
	if err := repo.Fail(ctx, dead.JobID, errors.New("boom"), 1); err != nil {
		t.Fatal(err)
	}

	depth, err := repo.Depth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth["embed"].Pending != 3 || depth["graph_extract"].Pending != 1 {
		t.Errorf("pending = %v", depth)
	}
	if depth["embed"].Dead != 1 {
		t.Errorf("dead = %v, want embed:1", depth)
	}
}

func TestJobRepoReclaimRequeuesAbandonedJob(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	// Simule un worker mort depuis longtemps: updated_at (posé par Claim)
	// est repoussé loin dans le passé, bien au-delà du seuil utilisé plus
	// bas.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE job_id = $1`,
		j.JobID); err != nil {
		t.Fatal(err)
	}

	n, err := repo.Reclaim(ctx, 100*time.Millisecond, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Reclaim a repris %d job(s), want 1", n)
	}

	var status string
	var attempts int
	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts, last_error FROM jobs WHERE job_id = $1`, j.JobID).
		Scan(&status, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if lastError == nil || *lastError == "" {
		t.Error("last_error doit être renseigné après une reprise")
	}
}

func TestJobRepoReclaimDeadLettersAtRetryLimit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	const retryLimit = 3
	// Déjà à une tentative de la limite: la reprise doit l'achever plutôt
	// que de le remettre en attente.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET attempts = $2, updated_at = now() - interval '1 hour'
		 WHERE job_id = $1`, j.JobID, retryLimit-1); err != nil {
		t.Fatal(err)
	}

	n, err := repo.Reclaim(ctx, 100*time.Millisecond, retryLimit)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Reclaim a repris %d job(s), want 1", n)
	}

	var status string
	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM jobs WHERE job_id = $1`, j.JobID).
		Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Errorf("status = %q, want dead: la reprise a épuisé retryLimit", status)
	}
	if attempts != retryLimit {
		t.Errorf("attempts = %d, want %d", attempts, retryLimit)
	}
}

// TestJobRepoReclaimIgnoresFreshlyClaimedJob est le test qui prouve que le
// seuil est respecté: un job réclamé à l'instant, dont le worker est encore
// vivant en train de le traiter, ne doit surtout pas se faire voler par
// Reclaim. Un reaper qui volerait des jobs vivants serait pire que pas de
// reaper du tout.
func TestJobRepoReclaimIgnoresFreshlyClaimedJob(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	// Pas de vieillissement ici: updated_at reste celui posé par Claim,
	// c'est-à-dire maintenant.
	n, err := repo.Reclaim(ctx, time.Hour, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Reclaim a repris %d job(s), want 0: le job est encore frais", n)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE job_id = $1`, j.JobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running: un job vivant ne doit pas être touché", status)
	}
}

// TestJobRepoCompleteFailsIfNoLongerHeld couvre le cas d'un worker lent mais
// vivant qui se fait reclaimer (un autre worker a déjà repris le job, qui
// est donc reparti en pending, voire déjà repris par un troisième), puis
// qui finit enfin et appelle Complete: Complete ne doit pas écraser l'état
// du job que quelqu'un d'autre possède peut-être en ce moment même.
func TestJobRepoCompleteFailsIfNoLongerHeld(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	// Simule une reprise par Reclaim pendant que ce worker croit encore
	// tenir le job.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET status = 'pending' WHERE job_id = $1`, j.JobID); err != nil {
		t.Fatal(err)
	}

	if err := repo.Complete(ctx, j.JobID); !errors.Is(err, ErrJobNotHeld) {
		t.Errorf("Complete = %v, want ErrJobNotHeld", err)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE job_id = $1`, j.JobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending: Complete ne doit pas écraser un job repris par un autre worker", status)
	}
}

// TestJobRepoFailFailsIfNoLongerHeld est le test qui compte le plus dans ce
// couple: sans la garde, le Fail perdant d'un worker qui ne tient plus le
// job gonflerait quand même attempts et pousserait un job par ailleurs sain
// vers la lettre morte.
func TestJobRepoFailFailsIfNoLongerHeld(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET status = 'pending' WHERE job_id = $1`, j.JobID); err != nil {
		t.Fatal(err)
	}

	if err := repo.Fail(ctx, j.JobID, errors.New("boom"), 5); !errors.Is(err, ErrJobNotHeld) {
		t.Errorf("Fail = %v, want ErrJobNotHeld", err)
	}

	var status string
	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM jobs WHERE job_id = $1`, j.JobID).
		Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0: Fail ne doit pas gonfler le compteur d'un job qu'il ne tient plus", attempts)
	}
}

// TestJobRepoReclaimSubSecondThresholdDoesNotReclaimFreshJob épingle le
// défaut du seuil sous la seconde: fmt.Sprintf("%d seconds",
// int(olderThan.Seconds())) tronquait tout ce qui est sous la seconde à
// "0 seconds", ce qui réduisait le prédicat à updated_at < now(), vrai pour
// n'importe quel job running puisque Claim l'a posé dans une transaction
// déjà commitée. TestJobRepoReclaimIgnoresFreshlyClaimedJob ne peut pas voir
// ça: son seuil (100ms) dégénère déjà en zéro lui aussi, et il ne passe que
// parce que son job y est vieilli d'une heure malgré tout. Ici rien n'est
// vieilli: c'est le job tout juste réclamé, sans artifice, contre un seuil
// sous la seconde.
func TestJobRepoReclaimSubSecondThresholdDoesNotReclaimFreshJob(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	n, err := repo.Reclaim(ctx, 500*time.Millisecond, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Reclaim a repris %d job(s), want 0: un seuil sous la seconde ne doit pas reprendre un job tout juste réclamé", n)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE job_id = $1`, j.JobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running: un job tout juste réclamé ne doit pas être touché", status)
	}
}

// TestJobRepoFailSetsExponentialBackoffOnFirstFailure vérifie que le
// premier échec repousse run_after d'environ deux secondes (2^1), pour que
// le calcul du backoff, déplacé en SQL, reste correct plutôt que
// silencieusement cassé par un futur changement de la formule.
func TestJobRepoFailSetsExponentialBackoffOnFirstFailure(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}

	before := time.Now()
	if err := repo.Fail(ctx, j.JobID, errors.New("boom"), 5); err != nil {
		t.Fatal(err)
	}

	var runAfter time.Time
	if err := pool.QueryRow(ctx,
		`SELECT run_after FROM jobs WHERE job_id = $1`, j.JobID).Scan(&runAfter); err != nil {
		t.Fatal(err)
	}
	delay := runAfter.Sub(before)
	if delay < 1500*time.Millisecond || delay > 3*time.Second {
		t.Errorf("backoff après le premier échec = %v, want ~2s (2^1)", delay)
	}
}

// TestJobRepoReclaimPreservesRealFailureCause couvre le cas d'un job qui a
// déjà échoué deux fois pour une vraie raison, puis dont le worker meurt
// pendant la troisième tentative: Reclaim ne doit pas écraser ce diagnostic
// avec sa propre note, sans quoi l'opérateur perdrait la seule cause utile
// au profit d'un message générique "worker abandoned".
func TestJobRepoReclaimPreservesRealFailureCause(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}
	if err := repo.Fail(ctx, j.JobID, errors.New("ollama down"), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET run_after = now() WHERE job_id = $1`, j.JobID); err != nil {
		t.Fatal(err)
	}

	j2, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j2 == nil {
		t.Fatal("Claim doit rendre le job posé une seconde fois")
	}
	// Le worker qui vient de le réclamer meurt: on vieillit updated_at pour
	// simuler l'abandon puis on le reprend.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE job_id = $1`,
		j2.JobID); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.Reclaim(ctx, 100*time.Millisecond, 10); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("Reclaim a repris %d job(s), want 1", n)
	}

	var lastError *string
	if err := pool.QueryRow(ctx,
		`SELECT last_error FROM jobs WHERE job_id = $1`, j.JobID).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError == nil || !strings.Contains(*lastError, "ollama down") {
		t.Errorf("last_error = %v, want la cause d'origine préservée", lastError)
	}
}

// TestJobRepoClaimIsExclusiveUnderConcurrency vérifie l'invariant dont tout
// le mécanisme dépend: sous contention réelle (plusieurs connexions,
// plusieurs goroutines), FOR UPDATE SKIP LOCKED ne laisse jamais deux
// workers repartir avec le même job. Contrairement à
// TestJobRepoEnqueueAndClaim, qui réclame deux fois depuis une seule
// goroutine et ne prouve donc que le filtre de statut, ce test-ci force la
// contention.
func TestJobRepoClaimIsExclusiveUnderConcurrency(t *testing.T) {
	pool := newTestPoolWithConns(t, 8)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	const n = 30
	for i := 0; i < n; i++ {
		if err := repo.Enqueue(ctx, "embed", "ws1", "c", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[int64]int{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := repo.Claim(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.JobID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("%d jobs distincts vus, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("job %d réclamé %d fois, want 1", id, count)
		}
	}
}

// TestJobRepoReclaimIsSafeUnderConcurrency vérifie le second invariant de
// concurrence: plusieurs Reclaim concurrents sur la même ligne abandonnée
// ne doivent la reprendre qu'une seule fois. La ligne n'est verrouillée que
// par la durée de l'UPDATE (pas de transaction explicite), donc seul le
// premier UPDATE à s'exécuter voit encore status = 'running'; les suivants,
// une fois débloqués, relisent une ligne déjà pending et n'affectent rien.
func TestJobRepoReclaimIsSafeUnderConcurrency(t *testing.T) {
	pool := newTestPoolWithConns(t, 8)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("Claim doit rendre le job posé")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE job_id = $1`,
		j.JobID); err != nil {
		t.Fatal(err)
	}

	const attempts = 5
	var wg sync.WaitGroup
	var total atomic.Int64
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := repo.Reclaim(ctx, 100*time.Millisecond, 10)
			if err != nil {
				t.Error(err)
				return
			}
			total.Add(n)
		}()
	}
	wg.Wait()

	if total.Load() != 1 {
		t.Errorf("total repris = %d, want 1: une seule reprise doit réussir", total.Load())
	}

	var gotAttempts int
	if err := pool.QueryRow(ctx,
		`SELECT attempts FROM jobs WHERE job_id = $1`, j.JobID).Scan(&gotAttempts); err != nil {
		t.Fatal(err)
	}
	if gotAttempts != 1 {
		t.Errorf("attempts = %d, want 1: des Reclaim concurrents ne doivent pas compter plusieurs fois", gotAttempts)
	}
}

// TestJobRepoFailTruncatesOnRuneBoundary : une cause d'échec en français qui
// dépasse la limite de last_error au milieu d'un caractère accentué produisait
// de l'UTF-8 invalide, que Postgres refuse. L'UPDATE échouait alors
// entièrement: le job restait bloqué en running jusqu'à ce que le reaper le
// reprenne, en perdant au passage la vraie cause.
func TestJobRepoFailTruncatesOnRuneBoundary(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewJobRepo(pool)

	if err := repo.Enqueue(ctx, "embed", "ws1", "conv_1",
		map[string]any{"message_id": uuid.New()}); err != nil {
		t.Fatal(err)
	}
	j, err := repo.Claim(ctx)
	if err != nil || j == nil {
		t.Fatalf("claim: %v, job = %v", err, j)
	}

	// L'octet 2000 tombe au milieu du "é": une coupe à l'octet produirait
	// une chaîne invalide.
	cause := errors.New(strings.Repeat("a", 1999) + "é" + strings.Repeat("b", 100))
	if err := repo.Fail(ctx, j.JobID, cause, 5); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT last_error FROM jobs WHERE job_id = $1`, j.JobID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(stored) {
		t.Error("last_error doit rester de l'UTF-8 valide")
	}
	if len(stored) > 2000 {
		t.Errorf("last_error fait %d octets, want <= 2000", len(stored))
	}
	if stored != strings.Repeat("a", 1999) {
		t.Errorf("la coupe doit retirer la rune coupée en deux, pas la tronquer: "+
			"%d octets, se termine par %q", len(stored), stored[len(stored)-3:])
	}
}
