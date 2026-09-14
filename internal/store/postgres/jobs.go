package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// ErrJobNotHeld signale qu'un worker a appelé Complete ou Fail sur un job
// qu'il ne tient plus: un autre worker l'a entre-temps repris, en général
// parce que Reclaim l'a jugé abandonné alors que celui-ci était en fait
// juste lent. Ni Complete ni Fail n'ont alors touché la ligne: écraser
// l'état d'un job qu'un autre worker possède peut-être activement
// corromprait la file (un Fail perdant gonflerait attempts pour de bon, un
// Complete perdant écraserait un second traitement en cours).
//
// Ambiguïté assumée: un job_id inexistant produit aussi ErrJobNotHeld (le
// WHERE ne matche aucune ligne dans les deux cas), et le runner attribuerait
// alors à tort la perte à ReclaimAfter. Rien ne supprime jamais de ligne de
// jobs aujourd'hui, donc ce second cas ne se produit pas en pratique; s'il
// devient possible (purge, TTL...), les deux causes devront être distinguées
// avant que ce message ne devienne trompeur.
var ErrJobNotHeld = errors.New("job is no longer held by this worker")

// Job est un job de la file, tel que réclamé par un worker.
type Job struct {
	JobID          int64
	JobType        string
	WorkspaceID    string
	ConversationID string
	Payload        json.RawMessage
	Attempts       int
}

// JobRepo implémente memory.JobQueue sur PostgreSQL.
type JobRepo struct{ pool *pgxpool.Pool }

func NewJobRepo(pool *pgxpool.Pool) *JobRepo { return &JobRepo{pool: pool} }

func (r *JobRepo) Enqueue(ctx context.Context, jobType, workspaceID,
	conversationID string, payload any) error {

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO jobs (job_type, workspace_id, conversation_id, payload)
		VALUES ($1, $2, $3, $4)`, jobType, workspaceID, conversationID, body)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", jobType, err)
	}
	return nil
}

// Claim réclame un job prêt. SKIP LOCKED laisse les autres workers avancer sur
// les jobs voisins au lieu d'attendre celui-ci. Rend nil quand rien n'est prêt.
func (r *JobRepo) Claim(ctx context.Context) (*Job, error) {
	var j Job
	err := r.pool.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', updated_at = now()
		WHERE job_id = (
			SELECT job_id FROM jobs
			WHERE status = 'pending' AND run_after <= now()
			ORDER BY run_after, job_id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING job_id, job_type, workspace_id, conversation_id, payload, attempts`,
	).Scan(&j.JobID, &j.JobType, &j.WorkspaceID, &j.ConversationID,
		&j.Payload, &j.Attempts)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &j, nil
}

// Complete marque un job terminé. La clause status = 'running' n'est pas
// redondante: sans elle, un worker lent mais vivant, reclaimé par erreur
// pendant qu'il travaillait encore, écraserait silencieusement l'état d'un
// job qu'un second worker est peut-être en train de traiter. Rend
// ErrJobNotHeld dans ce cas, sans toucher la ligne.
func (r *JobRepo) Complete(ctx context.Context, jobID int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs SET status = 'done', updated_at = now()
		WHERE job_id = $1 AND status = 'running'`, jobID)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotHeld
	}
	return nil
}

// Fail incrémente le compteur et repousse le job, ou le tue si la limite est
// atteinte. Un job mort reste en base: c'est la file de lettres mortes.
//
// Tout se fait dans un seul UPDATE: lire attempts puis écrire dans deux
// requêtes séparées serait une course (lecture-modification-écriture) si
// deux appelants touchaient la même ligne en même temps, ce qui devient
// possible depuis que Reclaim existe à côté du worker normal. Le calcul du
// backoff (exponentiel, plafonné à 5 minutes) passe donc en SQL plutôt qu'en
// Go, sur la valeur post-incrément d'attempts.
//
// La clause status = 'running' garde le même rôle que dans Complete: sans
// elle, le Fail perdant d'un worker reclaimé par erreur gonflerait quand
// même attempts et pousserait un job par ailleurs sain vers la lettre
// morte. Rend ErrJobNotHeld sans toucher la ligne dans ce cas.
func (r *JobRepo) Fail(ctx context.Context, jobID int64, cause error, retryLimit int) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	// Coupe sur une frontière de rune, jamais à l'octet: une cause en
	// français qui dépasse la limite au milieu d'un caractère accentué
	// produirait de l'UTF-8 invalide, que Postgres refuse. L'UPDATE
	// échouerait alors entièrement, le job resterait bloqué en running
	// jusqu'à ce que le reaper le reprenne, et la vraie cause serait perdue
	// au passage.
	msg = memory.TruncateRunes(msg, 2000)

	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs SET
			attempts   = attempts + 1,
			last_error = $2,
			status     = CASE WHEN attempts + 1 >= $3 THEN 'dead' ELSE 'pending' END,
			run_after  = CASE WHEN attempts + 1 >= $3 THEN run_after
			                  ELSE now() + (least(power(2::numeric, attempts + 1), 300)::int::text
			                                || ' seconds')::interval END,
			updated_at = now()
		WHERE job_id = $1 AND status = 'running'`, jobID, msg, retryLimit)
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotHeld
	}
	return nil
}

// Reclaim remet en attente les jobs qu'un worker mort a laissés en running.
// Pour un job running, updated_at est l'instant où Claim l'a pris (Claim
// pose updated_at = now() dans le même UPDATE qui pose status = 'running'),
// donc un running plus vieux que olderThan signifie que plus personne ne le
// traite: le worker qui le tenait est mort (OOM kill, drain de noeud,
// SIGKILL pendant un déploiement) sans jamais appeler Complete ni Fail.
//
// olderThan passe en secondes flottantes à make_interval plutôt que par un
// gabarit "%d seconds" tronqué en Go: fmt.Sprintf("%d seconds",
// int(olderThan.Seconds())) réduisait tout seuil sous la seconde à
// "0 seconds", ce qui effondrait le prédicat en updated_at < now(), vrai
// pour absolument tout job running. Voir
// TestJobRepoReclaimSubSecondThresholdDoesNotReclaimFreshJob.
//
// Incrémente attempts et respecte retryLimit comme Fail: un job repris qui a
// épuisé ses tentatives part en 'dead' plutôt que de repartir indéfiniment.
// Sans ça, un job dont le handler tue systématiquement son worker serait
// repris en boucle pour toujours au lieu de finir en lettre morte comme
// n'importe quel job qui échoue de façon répétée.
//
// last_error est complété plutôt qu'écrasé: un job déjà tombé deux fois pour
// une vraie cause ne doit pas perdre ce diagnostic au profit d'une note
// générique de reprise, la seule perte que le worker mort a réellement
// causée étant l'absence de rapport, pas la cause du (ou des) échec(s)
// précédent(s).
//
// Contrairement à Fail, un job repris redevient immédiatement éligible
// (run_after = now()): ce n'est pas le job qui a échoué, c'est son worker
// qui a disparu, donc rien ne justifie d'attendre.
func (r *JobRepo) Reclaim(ctx context.Context, olderThan time.Duration, retryLimit int) (int64, error) {
	const note = "reclaimed: worker abandoned this job before reporting completion"
	tag, err := r.pool.Exec(ctx, `
		UPDATE jobs SET
			attempts   = attempts + 1,
			last_error = left(CASE WHEN last_error IS NULL OR last_error = '' THEN $2
			                       ELSE last_error || ' | ' || $2 END, 4000),
			status     = CASE WHEN attempts + 1 >= $3 THEN 'dead' ELSE 'pending' END,
			run_after  = now(),
			updated_at = now()
		WHERE status = 'running' AND updated_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds(), note, retryLimit)
	if err != nil {
		return 0, fmt.Errorf("reclaim jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// JobDepth compte, pour un type de job, les jobs en attente (pending) et en
// lettre morte (dead), séparément: une file saine a un pending qui varie
// librement et un dead qui reste à zéro, ce ne sont pas deux nombres que
// l'on veut voir fondus en un seul.
type JobDepth struct {
	Pending int
	Dead    int
}

// Depth rend, par type de job, le compte pending et le compte dead, pour
// /debug/stats. Le compte dead n'existait pas avant le mécanisme de reprise
// de cette tâche; c'est pourtant le chiffre qu'un opérateur regarde en
// premier après un incident, pour savoir si des messages ont fini
// définitivement non indexés.
func (r *JobRepo) Depth(ctx context.Context) (map[string]JobDepth, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT job_type, status, count(*) FROM jobs
		WHERE status IN ('pending', 'dead') GROUP BY job_type, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]JobDepth{}
	for rows.Next() {
		var t, status string
		var n int
		if err := rows.Scan(&t, &status, &n); err != nil {
			return nil, err
		}
		d := out[t]
		switch status {
		case "pending":
			d.Pending = n
		case "dead":
			d.Dead = n
		}
		out[t] = d
	}
	return out, rows.Err()
}
