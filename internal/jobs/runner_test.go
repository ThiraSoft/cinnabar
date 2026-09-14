package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

// fakeClaimer remplace le repo pour tester la boucle sans Postgres. mu
// protège pending: Claim mute la slice, et rien ne garantit qu'un seul
// worker l'appelle à la fois dès que Workers > 1 ou que le reclaimLoop (qui
// tourne dans sa propre goroutine dès Run) est actif en même temps.
type fakeClaimer struct {
	mu        sync.Mutex
	pending   []*postgres.Job
	completed atomic.Int64
	failed    atomic.Int64
	lastErr   atomic.Value
	reclaimed atomic.Int64

	// completeErr, quand il est non nil, est renvoyé tel quel par Complete
	// au lieu de compter un succès: sert à simuler un job repris par un
	// autre worker (postgres.ErrJobNotHeld) pendant que celui-ci croyait
	// encore le tenir.
	completeErr error
}

func (f *fakeClaimer) Claim(context.Context) (*postgres.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil, nil
	}
	j := f.pending[0]
	f.pending = f.pending[1:]
	return j, nil
}
func (f *fakeClaimer) Complete(context.Context, int64) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	f.completed.Add(1)
	return nil
}
func (f *fakeClaimer) Fail(_ context.Context, _ int64, cause error, _ int) error {
	f.failed.Add(1)
	f.lastErr.Store(cause.Error())
	return nil
}

// Reclaim fait partie de Queue (voir runner.go): fakeClaimer doit
// l'implémenter pour satisfaire l'interface, quel que soit le test qui
// l'utilise. Rend 0 par défaut; les tests qui veulent en observer les
// appels lisent reclaimed.
func (f *fakeClaimer) Reclaim(context.Context, time.Duration, int) (int64, error) {
	f.reclaimed.Add(1)
	return 0, nil
}

func TestRunnerCompletesJobs(t *testing.T) {
	f := &fakeClaimer{pending: []*postgres.Job{
		{JobID: 1, JobType: "embed"},
		{JobID: 2, JobType: "embed"},
	}}
	var ran atomic.Int64
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{
		"embed": func(context.Context, *postgres.Job) error {
			ran.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if ran.Load() != 2 {
		t.Errorf("%d jobs traités, want 2", ran.Load())
	}
	if f.completed.Load() != 2 {
		t.Errorf("%d Complete, want 2", f.completed.Load())
	}
}

func TestRunnerFailsOnHandlerError(t *testing.T) {
	f := &fakeClaimer{pending: []*postgres.Job{{JobID: 1, JobType: "embed"}}}
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{
		"embed": func(context.Context, *postgres.Job) error {
			return errors.New("ollama down")
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if f.failed.Load() != 1 {
		t.Errorf("%d Fail, want 1", f.failed.Load())
	}
	if got, _ := f.lastErr.Load().(string); got != "ollama down" {
		t.Errorf("cause = %q", got)
	}
}

func TestRunnerFailsUnknownJobType(t *testing.T) {
	f := &fakeClaimer{pending: []*postgres.Job{{JobID: 1, JobType: "graph_extract"}}}
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	// Un type sans handler ne doit pas bloquer la file ni faire paniquer le
	// worker: il échoue proprement et part en backoff.
	if f.failed.Load() != 1 {
		t.Errorf("%d Fail, want 1", f.failed.Load())
	}
}

func TestRunnerStopsOnContextCancel(t *testing.T) {
	f := &fakeClaimer{}
	r := NewRunner(f, config.Jobs{Workers: 2, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run doit rendre la main à l'annulation du contexte")
	}
}

// TestRunnerReclaimsPeriodicallyAndStopsOnCancel prouve que Run déclenche
// la reprise périodique, maintenant que Reclaim fait partie de Queue plutôt
// que d'être une capacité optionnelle détectée par assertion de type: rien
// ne peut donc démarrer sans elle en silence.
func TestRunnerReclaimsPeriodicallyAndStopsOnCancel(t *testing.T) {
	f := &fakeClaimer{}
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond, ReclaimAfter: time.Minute},
		map[string]Handler{})
	r.reclaimTick = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Laisse le ticker (5ms) déclencher Reclaim au moins une fois avant
	// d'annuler.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run doit rendre la main à l'annulation du contexte, même avec le reclaimLoop actif")
	}

	if f.reclaimed.Load() == 0 {
		t.Error("Reclaim doit être appelé périodiquement")
	}
}

// TestRunnerLogsWhenJobReclaimedByAnotherWorker couvre le cas d'un worker
// lent mais vivant: son handler réussit, mais Complete rapporte que le job
// n'est plus tenu (postgres.ErrJobNotHeld), parce qu'un autre worker l'a
// entre-temps repris. Le runner ne doit ni le retenter via Fail (il n'y a
// rien à retenter, un autre worker possède désormais le job) ni se bloquer:
// Run doit toujours rendre la main proprement à l'annulation du contexte.
func TestRunnerLogsWhenJobReclaimedByAnotherWorker(t *testing.T) {
	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	f := &fakeClaimer{
		pending:     []*postgres.Job{{JobID: 42, JobType: "embed"}},
		completeErr: postgres.ErrJobNotHeld,
	}
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{
		"embed": func(context.Context, *postgres.Job) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Laisse le worker traiter le job et essuyer le Complete perdant avant
	// d'annuler.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run doit rendre la main à l'annulation du contexte, même après un Complete perdu")
	}

	if f.failed.Load() != 0 {
		t.Error("un job dont Complete rapporte qu'il n'est plus tenu ne doit pas être retenté via Fail: il n'y a rien à retenter")
	}
	out := logs.String()
	if !strings.Contains(out, "job_id=42") {
		t.Errorf("le journal doit identifier le job: %s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("la perte du job doit être journalisée en warn, pas en erreur: %s", out)
	}
	if !strings.Contains(out, "ReclaimAfter") {
		t.Errorf("le message doit pointer vers config.Jobs.ReclaimAfter pour être actionnable: %s", out)
	}
}

// blockingReclaimQueue simule un Reclaim en vol au moment de l'annulation:
// il bloque jusqu'à ce que le contexte qu'on lui passe soit terminé, comme
// le ferait un appel pgx interrompu en plein balayage, puis rend son
// erreur.
type blockingReclaimQueue struct {
	*fakeClaimer
	entered     chan struct{}
	enteredOnce sync.Once
}

func (f *blockingReclaimQueue) Reclaim(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	f.enteredOnce.Do(func() { close(f.entered) })
	<-ctx.Done()
	return 0, ctx.Err()
}

// TestRunnerReclaimLoopDoesNotLogErrorOnGracefulShutdown couvre le cas où
// l'annulation du contexte survient pendant que Reclaim est en vol: avec le
// scan complet de la table (voir la migration 003), ce recouvrement devient
// probable plutôt qu'anecdotique. Ce n'est pas une panne du reaper, donc ça
// ne doit pas produire de ligne ERROR à chaque extinction propre du
// service.
func TestRunnerReclaimLoopDoesNotLogErrorOnGracefulShutdown(t *testing.T) {
	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	f := &blockingReclaimQueue{fakeClaimer: &fakeClaimer{}, entered: make(chan struct{})}
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond, ReclaimAfter: time.Minute},
		map[string]Handler{})
	r.reclaimTick = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reclaimLoop n'a jamais appelé Reclaim")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run doit rendre la main même si Reclaim était en vol au moment de l'annulation")
	}

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("l'annulation pendant un balayage ne doit pas être journalisée comme une erreur: %s", logs.String())
	}
}

// TestRunnerMultipleWorkersContendSafely fait tourner plusieurs workers
// contre une file partagée sous -race: avant que fakeClaimer ne gagne son
// mutex, plusieurs workers (ou un worker et le reclaimLoop, désormais
// toujours actif) mutant f.pending sans protection auraient rendu ce test
// une course de données détectée par -race, plutôt qu'un simple risque
// théorique. Elle vérifie en plus que chaque job posé est bien traité
// exactement une fois, comme TestJobRepoClaimIsExclusiveUnderConcurrency le
// fait côté Postgres.
func TestRunnerMultipleWorkersContendSafely(t *testing.T) {
	const n = 40
	pending := make([]*postgres.Job, n)
	for i := range pending {
		pending[i] = &postgres.Job{JobID: int64(i + 1), JobType: "embed"}
	}
	f := &fakeClaimer{pending: pending}

	var mu sync.Mutex
	seen := map[int64]int{}
	r := NewRunner(f, config.Jobs{Workers: 8, RetryLimit: 3,
		PollInterval: 2 * time.Millisecond}, map[string]Handler{
		"embed": func(_ context.Context, j *postgres.Job) error {
			mu.Lock()
			seen[j.JobID]++
			mu.Unlock()
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if int64(len(seen)) != n {
		t.Errorf("%d jobs distincts traités, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("job %d traité %d fois, want 1", id, count)
		}
	}
	if f.completed.Load() != n {
		t.Errorf("%d Complete, want %d", f.completed.Load(), n)
	}
}

// deadlineCapturingQueue capture la deadline du contexte que Fail reçoit,
// pour prouver de quel budget elle dérive sans avoir à attendre les 5
// minutes réelles de config.JobHandlerTimeout.
type deadlineCapturingQueue struct {
	*fakeClaimer
	failDeadline    time.Time
	failHasDeadline bool
}

func (f *deadlineCapturingQueue) Fail(ctx context.Context, jobID int64, cause error, retryLimit int) error {
	f.failDeadline, f.failHasDeadline = ctx.Deadline()
	return f.fakeClaimer.Fail(ctx, jobID, cause, retryLimit)
}

// TestRunnerFailGetsIndependentBookkeepingDeadline prouve que Fail reçoit
// un contexte borné par bookkeepingTimeout (30s), pas par jobCtx et son
// config.JobHandlerTimeout (5 minutes) partagé avec le handler: un handler
// qui termine juste à sa propre échéance ne doit pas hériter d'un contexte
// déjà expiré pour enregistrer pourquoi. On ne peut pas attendre les 5
// vraies minutes de config.JobHandlerTimeout dans un test rapide, donc on
// inspecte la deadline plutôt que d'attendre qu'elle expire: si handle()
// repassait un jour jobCtx directement à Fail, cette deadline se
// retrouverait à environ 5 minutes de l'appel plutôt qu'à 30 secondes.
func TestRunnerFailGetsIndependentBookkeepingDeadline(t *testing.T) {
	f := &deadlineCapturingQueue{fakeClaimer: &fakeClaimer{
		pending: []*postgres.Job{{JobID: 1, JobType: "embed"}},
	}}
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{
		"embed": func(context.Context, *postgres.Job) error {
			return errors.New("boom")
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	before := time.Now()
	r.Run(ctx)

	if !f.failHasDeadline {
		t.Fatal("Fail doit recevoir un contexte avec deadline")
	}
	until := f.failDeadline.Sub(before)
	if until < 20*time.Second || until > 40*time.Second {
		t.Errorf("deadline de Fail = %v après l'appel, want ~30s (bookkeepingTimeout) pas ~%v (config.JobHandlerTimeout)",
			until, config.JobHandlerTimeout)
	}
}

// TestRunnerSurvivesAPanickingHandler : le même processus sert l'API et fait
// tourner les workers. Un handler qui panique sans filet emportait donc le
// serveur HTTP avec lui, pour un job. La panique doit devenir un échec de
// job comme un autre, avec sa valeur pour cause, et la boucle doit continuer
// puis rendre la main à l'annulation.
func TestRunnerSurvivesAPanickingHandler(t *testing.T) {
	f := &fakeClaimer{pending: []*postgres.Job{
		{JobID: 1, JobType: "embed"},
		{JobID: 2, JobType: "embed"},
	}}
	var seen atomic.Int64
	r := NewRunner(f, config.Jobs{Workers: 1, RetryLimit: 3,
		PollInterval: 5 * time.Millisecond}, map[string]Handler{
		"embed": func(_ context.Context, j *postgres.Job) error {
			if j.JobID == 1 {
				panic("index out of range [3] with length 2")
			}
			seen.Add(1)
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Run doit rendre la main de lui-même à l'expiration du contexte: si la
	// panique remontait, ce test mourrait avant d'arriver aux assertions.
	r.Run(ctx)

	if f.failed.Load() != 1 {
		t.Errorf("%d Fail, want 1: une panique doit échouer le job", f.failed.Load())
	}
	if got, _ := f.lastErr.Load().(string); !strings.Contains(got, "index out of range") {
		t.Errorf("cause = %q, want la valeur de la panique", got)
	}
	if seen.Load() != 1 || f.completed.Load() != 1 {
		t.Errorf("le worker doit avoir traité le job suivant: %d traités, %d Complete",
			seen.Load(), f.completed.Load())
	}
}
