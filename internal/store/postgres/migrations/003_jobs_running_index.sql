-- Reclaim balaie status = 'running' AND updated_at < seuil une fois par
-- minute, dans chaque worker: sans index dédié, ce balayage est un scan
-- séquentiel de toute la table jobs, qui ne rétrécit jamais (Complete
-- conserve les lignes terminées, rien ne les supprime). Même principe que
-- jobs_pending_idx dans 001_init.sql, côté file d'attente plutôt que côté
-- reprise.
CREATE INDEX jobs_running_idx ON jobs (updated_at) WHERE status = 'running';
