-- Extraction du graphe par fenêtre de messages plutôt que par message.
--
-- graph_extracted_seq est la séquence du dernier message soumis à
-- l'extracteur. Les séquences commencent à 1, donc 0 veut dire que rien n'a
-- encore été extrait.
ALTER TABLE conversations ADD COLUMN graph_extracted_seq BIGINT NOT NULL DEFAULT 0;

-- Les conversations existantes repartent juste avant le plus ancien message
-- dont un job graph_extract attend encore, et sont sinon considérées comme
-- extraites en entier. Les jobs morts restent morts: leurs messages
-- n'étaient déjà pas dans le graphe et ne le deviennent pas en silence.
UPDATE conversations c SET graph_extracted_seq = COALESCE(
    (SELECT min(m.sequence_number) - 1
       FROM jobs j
       JOIN messages m ON m.message_id::text = j.payload->>'message_id'
      WHERE j.job_type = 'graph_extract'
        AND j.status IN ('pending', 'running')
        AND j.conversation_id = c.conversation_id),
    c.next_sequence);

-- Le job en attente d'une conversation est retrouvé à chaque message écrit,
-- et Claim vérifie à chaque réclamation qu'aucune extraction de la même
-- conversation ne tourne déjà.
CREATE INDEX jobs_graph_pending_conv_idx ON jobs (conversation_id)
    WHERE job_type = 'graph_extract' AND status = 'pending';
CREATE INDEX jobs_graph_running_conv_idx ON jobs (conversation_id)
    WHERE job_type = 'graph_extract' AND status = 'running';
