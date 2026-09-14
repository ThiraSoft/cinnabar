CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE conversations (
    conversation_id TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL,
    scope           TEXT NOT NULL DEFAULT 'participants'
                    CHECK (scope IN ('private','participants','workspace','explicit')),
    next_sequence   BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

CREATE INDEX conversations_workspace_idx
    ON conversations (workspace_id) WHERE deleted_at IS NULL;

CREATE TABLE conversation_participants (
    conversation_id TEXT NOT NULL
        REFERENCES conversations(conversation_id) ON DELETE CASCADE,
    participant_key TEXT NOT NULL,
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    left_at         TIMESTAMPTZ,
    PRIMARY KEY (conversation_id, participant_key)
);

CREATE INDEX conversation_participants_key_idx
    ON conversation_participants (participant_key) WHERE left_at IS NULL;

CREATE TABLE messages (
    message_id      UUID PRIMARY KEY,
    conversation_id TEXT NOT NULL
        REFERENCES conversations(conversation_id) ON DELETE CASCADE,
    workspace_id    TEXT NOT NULL,
    sequence_number BIGINT NOT NULL,
    author_key      TEXT NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('user','assistant','system','tool')),
    content         TEXT NOT NULL,
    request_id      TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    edited_at       TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    metadata        JSONB NOT NULL DEFAULT '{}',
    tsv             TSVECTOR GENERATED ALWAYS AS
                    (to_tsvector('french'::regconfig, content)) STORED,
    UNIQUE (conversation_id, sequence_number),
    UNIQUE (conversation_id, request_id)
);

CREATE INDEX messages_tsv_idx ON messages USING GIN (tsv);
CREATE INDEX messages_conv_seq_idx ON messages (conversation_id, sequence_number);

CREATE TABLE memory_units (
    memory_unit_id     UUID PRIMARY KEY,
    workspace_id       TEXT NOT NULL,
    conversation_id    TEXT NOT NULL
        REFERENCES conversations(conversation_id) ON DELETE CASCADE,
    anchor_message_id  UUID NOT NULL
        REFERENCES messages(message_id) ON DELETE CASCADE,
    start_sequence     BIGINT NOT NULL,
    end_sequence       BIGINT NOT NULL,
    embedding_text     TEXT NOT NULL,
    embedding          VECTOR(768),
    embedding_model    TEXT NOT NULL,
    indexing_strategy  TEXT NOT NULL,
    indexing_version   INTEGER NOT NULL,
    scope              TEXT NOT NULL,
    active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (anchor_message_id, embedding_model, indexing_strategy, indexing_version)
);

CREATE INDEX memory_units_embedding_idx
    ON memory_units USING hnsw (embedding vector_cosine_ops) WHERE active;
CREATE INDEX memory_units_conv_idx
    ON memory_units (conversation_id) WHERE active;

CREATE TABLE memory_unit_acl (
    memory_unit_id UUID NOT NULL
        REFERENCES memory_units(memory_unit_id) ON DELETE CASCADE,
    principal_key  TEXT NOT NULL,
    permission     TEXT NOT NULL DEFAULT 'read',
    PRIMARY KEY (memory_unit_id, principal_key)
);

CREATE TABLE graph_entities (
    entity_id     UUID PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    canonical_key TEXT,
    entity_type   TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    aliases       JSONB NOT NULL DEFAULT '[]',
    resolved      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX graph_entities_canonical_idx
    ON graph_entities (workspace_id, canonical_key) WHERE canonical_key IS NOT NULL;
CREATE INDEX graph_entities_name_trgm_idx
    ON graph_entities USING GIN (display_name gin_trgm_ops);
CREATE INDEX graph_entities_aliases_idx
    ON graph_entities USING GIN (aliases jsonb_path_ops);

CREATE TABLE graph_relations (
    relation_id       UUID PRIMARY KEY,
    workspace_id      TEXT NOT NULL,
    source_entity_id  UUID NOT NULL
        REFERENCES graph_entities(entity_id) ON DELETE CASCADE,
    relation_type     TEXT NOT NULL,
    target_entity_id  UUID
        REFERENCES graph_entities(entity_id) ON DELETE CASCADE,
    target_literal    TEXT,
    observed_at       TIMESTAMPTZ NOT NULL,
    valid_from        TIMESTAMPTZ,
    valid_until       TIMESTAMPTZ,
    ingested_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    invalidated_at    TIMESTAMPTZ,
    confidence        REAL NOT NULL DEFAULT 1.0,
    scope             TEXT NOT NULL,
    dedup_key         TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((target_entity_id IS NULL) <> (target_literal IS NULL))
);

CREATE UNIQUE INDEX graph_relations_dedup_idx
    ON graph_relations (workspace_id, dedup_key);
CREATE INDEX graph_relations_source_idx
    ON graph_relations (source_entity_id) WHERE invalidated_at IS NULL;
CREATE INDEX graph_relations_target_idx
    ON graph_relations (target_entity_id) WHERE invalidated_at IS NULL;

CREATE TABLE graph_relation_sources (
    relation_id     UUID NOT NULL
        REFERENCES graph_relations(relation_id) ON DELETE CASCADE,
    message_id      UUID NOT NULL
        REFERENCES messages(message_id) ON DELETE CASCADE,
    conversation_id TEXT NOT NULL
        REFERENCES conversations(conversation_id) ON DELETE CASCADE,
    PRIMARY KEY (relation_id, message_id)
);

CREATE INDEX graph_relation_sources_message_idx
    ON graph_relation_sources (message_id);

CREATE TABLE jobs (
    job_id          BIGSERIAL PRIMARY KEY,
    job_type        TEXT NOT NULL
                    CHECK (job_type IN ('embed','graph_extract','graph_reeval')),
    workspace_id    TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','running','done','dead')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    run_after       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX jobs_pending_idx ON jobs (run_after) WHERE status = 'pending';

CREATE TABLE api_clients (
    client_id          UUID PRIMARY KEY,
    label              TEXT NOT NULL,
    token_sha256       BYTEA NOT NULL UNIQUE,
    workspace_id       TEXT NOT NULL,
    allowed_identities JSONB NOT NULL DEFAULT '[]',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at       TIMESTAMPTZ,
    revoked_at         TIMESTAMPTZ
);

CREATE INDEX api_clients_workspace_idx
    ON api_clients (workspace_id) WHERE revoked_at IS NULL;
