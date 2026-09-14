// Package jobs consomme la file de tâches asynchrones.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ThiraSoft/cinnabar/internal/config"
	"github.com/ThiraSoft/cinnabar/internal/store/postgres"
)

type Handler func(ctx context.Context, j *postgres.Job) error

// bookkeepingTimeout borne les appels à Complete et Fail, indépendamment du
// budget du handler (config.JobHandlerTimeout). Un handler qui court
// jusqu'à sa propre deadline ne doit pas hériter d'un contexte déjà expiré
// pour enregistrer son échec: sans ce contexte séparé, pgx rendrait la main
// immédiatement, la vraie cause ne serait jamais écrite dans last_error, et
// le job resterait bloqué en running jusqu'à ce que le reaper le reprenne,
// en écrasant au passage le peu de diagnostic qui aurait pu être écrit.
const bookkeepingTimeout = 30 * time.Second

// Queue est la part du repo de jobs dont le runner a besoin. L'interface est
// déclarée ici, chez le consommateur, pour que le runner se teste sans
// base. Reclaim en fait partie: c'est un filet de sécurité de production
// (repêcher les jobs abandonnés par un worker mort), pas une capacité
// optionnelle qu'une file pourrait légitimement omettre. Le laisser
// optionnel a longtemps rendu ce filet désactivable par une simple absence
// de méthode sur l'implémentation fournie, sans le moindre log ni la
// moindre erreur au démarrage pour le signaler; fakeClaimer, dans les
// tests, l'implémente donc aussi désormais (voir runner_test.go).
//
// Complete et Fail peuvent rendre postgres.ErrJobNotHeld: le job a été repris
// par un autre worker (Reclaim l'a jugé abandonné) pendant que celui-ci le
// traitait encore. handle le journalise à part, ce n'est pas une erreur à
// retenter.
type Queue interface {
	Claim(ctx context.Context) (*postgres.Job, error)
	Complete(ctx context.Context, jobID int64) error
	Fail(ctx context.Context, jobID int64, cause error, retryLimit int) error
	Reclaim(ctx context.Context, olderThan time.Duration, retryLimit int) (int64, error)
}

type Runner struct {
	queue    Queue
	cfg      config.Jobs
	handlers map[string]Handler

	// reclaimTick commande la fréquence de reclaimLoop. Champ plutôt que
	// variable de paquet: une variable globale mutable comme seam de test
	// force tous les tests du paquet à se coordonner sur un seul état
	// partagé, alors qu'un champ se règle par instance sans rien risquer
	// pour les autres tests qui tournent en parallèle.
	reclaimTick time.Duration
}

func NewRunner(queue Queue, cfg config.Jobs, handlers map[string]Handler) *Runner {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.RetryLimit <= 0 {
		cfg.RetryLimit = 5
	}
	if cfg.ReclaimAfter <= 0 {
		cfg.ReclaimAfter = 10 * time.Minute
	}
	return &Runner{queue: queue, cfg: cfg, handlers: handlers, reclaimTick: time.Minute}
}

// Run bloque jusqu'à l'annulation du contexte. Chaque worker finit le job
// qu'il tient avant de s'arrêter, ce qui donne un arrêt propre.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			r.loop(ctx, worker)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.reclaimLoop(ctx)
	}()
	wg.Wait()
}

// reclaimLoop récupère périodiquement les jobs qu'un worker mort a laissés
// en running. Une fois par minute est largement suffisant vis-à-vis de
// ReclaimAfter (10 minutes par défaut), qui laisse elle-même une marge
// confortable au-dessus de config.JobHandlerTimeout: un handler lent mais vivant ne
// doit jamais se faire voler son job. Un reaper silencieux doit le rester:
// on ne logue qu'un compte non nul.
func (r *Runner) reclaimLoop(ctx context.Context) {
	ticker := time.NewTicker(r.reclaimTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.queue.Reclaim(ctx, r.cfg.ReclaimAfter, r.cfg.RetryLimit)
			if err != nil {
				if ctx.Err() != nil {
					// L'annulation elle-même a fait échouer l'appel en vol
					// (contexte expiré au milieu du balayage): c'est un
					// arrêt propre, pas une panne du reaper, et le
					// journaliser en erreur polluerait chaque extinction.
					return
				}
				slog.Error("reclaim jobs failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Warn("reclaimed jobs abandoned by a dead worker", "count", n)
			}
		}
	}
}

func (r *Runner) loop(ctx context.Context, worker int) {
	for {
		if ctx.Err() != nil {
			return
		}
		j, err := r.queue.Claim(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("claim job failed", "worker", worker, "error", err)
			if !sleepCtx(ctx, r.cfg.PollInterval) {
				return
			}
			continue
		}
		if j == nil {
			if !sleepCtx(ctx, r.cfg.PollInterval) {
				return
			}
			continue
		}
		r.handle(ctx, j)
	}
}

func (r *Runner) handle(ctx context.Context, j *postgres.Job) {
	// Le job en cours utilise un contexte détaché de l'annulation, pour
	// pouvoir finir pendant l'arrêt propre. Le timeout borne l'attente.
	// Réservé au handler: fail et complete (ci-dessous) empruntent leur
	// propre contexte de bookkeeping, plus court et indépendant, pour
	// pouvoir écrire le résultat même quand ce timeout-ci vient d'expirer.
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), config.JobHandlerTimeout)
	defer cancel()

	h, ok := r.handlers[j.JobType]
	if !ok {
		// "graph_extract" et "graph_reeval" ont bien un handler (voir
		// jobs.GraphExtractHandler et jobs.GraphReevalHandler): cmd/cinnabar
		// ne les enregistre que si graph.enabled vaut true. Un déploiement
		// où le graphe est désactivé, ou qui vient de le redésactiver avec
		// un job de ce type encore en file, retombe donc ici: un type sans
		// handler enregistré ne doit ni faire paniquer le worker ni
		// bloquer la file, donc on échoue proprement le job, qui suivra le
		// circuit normal de retry puis de lettre morte.
		err := fmt.Errorf("no handler for job type %q", j.JobType)
		slog.Error("job rejected", "job_id", j.JobID, "type", j.JobType, "error", err)
		r.fail(ctx, j, err)
		return
	}

	start := time.Now()
	if err := r.call(h, jobCtx, j); err != nil {
		slog.Warn("job failed", "job_id", j.JobID, "type", j.JobType,
			"attempts", j.Attempts, "duration_ms", time.Since(start).Milliseconds(),
			"error", err)
		r.fail(ctx, j, err)
		return
	}
	r.complete(ctx, j)
}

// call exécute le handler et convertit une panique en erreur de job. Sans ce
// filet, un handler qui panique emportait tout le processus, donc aussi le
// serveur HTTP: l'API et les workers tournent dans le même binaire (voir
// cmd/cinnabar), et un seul job mal formé suffisait à rendre le service
// indisponible. Une panique redevient donc un échec de job ordinaire, avec
// sa valeur pour cause, qui suit le circuit normal de retry puis de lettre
// morte.
//
// Le journal est en erreur et pas en warning, contrairement à un échec
// rendu proprement: un handler qui panique est un bug, pas une dépendance
// en panne, et il ne doit pas se noyer dans le bruit des retries normaux.
// La pile n'est volontairement pas jointe au champ d'erreur, qui finit en
// last_error en base: elle est journalisée à part.
func (r *Runner) call(h Handler, ctx context.Context, j *postgres.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("job handler panicked", "job_id", j.JobID,
				"type", j.JobType, "panic", fmt.Sprint(p),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("job handler panicked: %v", p)
		}
	}()
	return h(ctx, j)
}

// fail et complete empruntent leur propre contexte de bookkeeping, dérivé
// de ctx (le contexte de Run, pas jobCtx): détaché de l'annulation comme
// jobCtx pour pouvoir écrire pendant un arrêt propre, mais avec son propre
// budget (bookkeepingTimeout) plutôt que d'hériter de celui, potentiellement
// déjà épuisé, du handler. Un handler qui vient de courir jusqu'à
// config.JobHandlerTimeout doit encore pouvoir écrire pourquoi.
func (r *Runner) fail(ctx context.Context, j *postgres.Job, cause error) {
	bkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	if err := r.queue.Fail(bkCtx, j.JobID, cause, r.cfg.RetryLimit); err != nil {
		r.reportQueueOutcome(j, "fail", err)
	}
}

func (r *Runner) complete(ctx context.Context, j *postgres.Job) {
	bkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	if err := r.queue.Complete(bkCtx, j.JobID); err != nil {
		r.reportQueueOutcome(j, "complete", err)
	}
}

// reportQueueOutcome journalise l'échec d'un Complete ou d'un Fail. Un
// postgres.ErrJobNotHeld n'est pas une vraie erreur: le job a changé de
// mains (Reclaim l'a jugé abandonné) pendant que ce worker le traitait
// encore, il n'y a rien à retenter, un autre worker le possède désormais.
// C'est le seul endroit où un opérateur apprend que config.Jobs.ReclaimAfter
// est trop agressif pour la durée réelle de ce handler, donc le message le
// dit explicitement plutôt que de se contenter du nom de l'erreur.
func (r *Runner) reportQueueOutcome(j *postgres.Job, action string, err error) {
	if errors.Is(err, postgres.ErrJobNotHeld) {
		slog.Warn("job reclaimed by another worker while still being processed; "+
			"nothing to retry, but consider raising config.Jobs.ReclaimAfter "+
			"if this handler genuinely needs longer",
			"job_id", j.JobID, "type", j.JobType, "action", action)
		return
	}
	slog.Error("mark job "+action+" failed", "job_id", j.JobID, "error", err)
}

// sleepCtx dort ou rend faux si le contexte est annulé pendant l'attente.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
