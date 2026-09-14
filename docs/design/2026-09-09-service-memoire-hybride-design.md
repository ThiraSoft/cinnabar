# Service de mémoire hybride pour agents IA, design validé

Date : 2026-09-09
Statut : validé
Nom de travail : cinnabar

## 1. Objectif et périmètre

Service Go autonome qui reçoit les messages de conversations entre agents ou entre un
utilisateur et un agent, en construit une mémoire consultable, et expose une recherche
qui rend des extraits de conversations passées autorisés.

Le service n'est pas un proxy de chat. Il n'appelle jamais de LLM sur le chemin d'une
requête de recherche. C'est l'orchestrateur d'agents qui parle au LLM de conversation et
qui injecte lui-même le bloc de mémoire dans son prompt.

Périmètre retenu : V1 et V2 de la spec source dans un seul cycle. La temporalité de la
V3 est partiellement embarquée parce que le modèle bi-temporel coûte peu à mettre en
place et évite une migration douloureuse plus tard. Le reranker, la détection automatique
du type de requête et la consolidation d'entités restent hors périmètre.

Niveau de finition : service destiné à la production, utilisé par plusieurs projets. Code
testé sur la logique métier, migrations SQL versionnées, docker-compose pour le
développement, image Docker multi-stage, arrêt propre, timeouts explicites.

Deux exclusions assumées. Pas de Prometheus, parce que le service reste petit et que des
compteurs JSON suffisent. Pas de sondes de santé normalisées non plus, seulement un
`/health` et un `/about` maison, ce qui devra être revu si l'infra cible en réclame.

## 2. Décisions structurantes

### 2.1 Architecture retenue

Monolithe modulaire, un seul binaire, tout dans Postgres. Postgres porte les messages, les
vecteurs via pgvector, l'index lexical via la recherche plein texte, les tables du graphe
et la file de jobs. Les workers tournent comme goroutines dans le même process et se
coupent par un flag.

L'alternative de deux binaires séparés a été écartée pour cette version. Le chemin vers
elle reste ouvert puisque les workers sont des composants appelés depuis `main` et non du code
noyé dans les handlers HTTP.

### 2.2 Graphiti

Écarté après vérification. Graphiti est du Python sans port Go, il exige Neo4j, FalkorDB
ou Neptune plutôt que Postgres, il embarque sa propre recherche hybride qui ferait doublon
avec la nôtre, et surtout son édition open source n'a que `group_id` pour partitionner,
donc aucun moyen de pousser notre prédicat d'ACL par participants dans sa recherche. Le
filtrage se ferait en post-traitement, ce qui dégrade le rappel et viole
`filter_before_retrieval`.

Ce qu'on lui emprunte : son modèle bi-temporel. Voir section 4.7.

Le port `GraphRepo` reste conçu pour qu'un adaptateur Graphiti soit branchable plus tard
sans toucher au domaine.

### 2.3 Identifiants

`workspace_id` et `conversation_id` sont du texte libre fourni par le client, donc
`conv_8453` est valide. Les identités restent des clés stables de la forme `user:<id>`,
`agent:<id>` ou `tool:<id>`.

Le service génère le `message_id` en UUIDv7 et attribue lui-même le `sequence_number`.
Le client n'a pas de compteur à tenir.

Le `memory_unit_id` reste un UUIDv5 déterministe sur l'ancre, le modèle, la stratégie et
la version, comme la section 9.3 de la spec source.

### 2.4 Authentification et isolation des workspaces

Chaque appelant présente un bearer token qui existe en base dans `api_clients`, stocké
sous forme de hash SHA-256. Le token porte son `workspace_id` et la liste des identités
qu'il peut incarner, avec support de motifs comme `agent:*`.

Le middleware résout le token en principal, puis refuse toute requête dont le
`workspace_id` ne correspond pas à celui du token, et toute requête dont l'`author_key` ou
le `requester_key` sort de la liste d'identités autorisées. C'est ce qui ferme l'isolation
des workspaces demandée en section 16 de la spec source, y compris face à un projet qui
tenterait de lire le workspace d'un autre en changeant un champ JSON.

L'ACL de lecture par participants et scope s'applique en plus, dans le SQL de la
recherche. Les deux mécanismes sont indépendants et se cumulent : le premier dit qui tu
peux prétendre être, le second dit ce que cette identité peut voir.

La gestion des clés passe par des sous-commandes du binaire, `cinnabar keys create` et
`cinnabar keys revoke`. Le token en clair ne s'affiche qu'à la création et n'est jamais
stocké.

### 2.5 Participants

Le premier message sur un `conversation_id` inconnu crée la conversation avec le scope par
défaut. Chaque `author_key` rencontré est ajouté aux participants s'il n'y est pas déjà.
`POST /v1/conversations` existe en optionnel pour déclarer un scope non par défaut ou des
participants passifs qui liront sans avoir écrit.

### 2.6 Sortie de la recherche

Aucun résumé LLM. La réponse est le JSON de la section 10 de la spec source, avec les
messages originaux cités mot pour mot et leurs `message_id` sources.

Un champ optionnel `context_block` assemble par template les faits du graphe et les
extraits qui les sourcent, prêt à injecter. Déterministe, non destructif, sans appel LLM.

### 2.7 Fournisseurs LLM

Un seul client OpenAI-compatible dans `internal/llm`, instancié deux fois.

L'embedder pointe sur Ollama en `/v1/embeddings` avec `nomic-embed-text-v2-moe`, vérifié
fonctionnel en local. Dimension 768, contexte 512 tokens.

L'extracteur de graphe pointe sur `${LLM_API_URL}` en
`/v1/chat/completions` avec `google/gemma-4-31B-it`, authentifié par `${LLM_API_KEY}`.
Le client accepte aussi des en-têtes arbitraires, pas seulement un bearer.

## 3. Structure du projet

```text
cinnabar/
  cmd/cinnabar/            main, flags --api et --workers
  internal/
    api/                 handlers, DTO, middleware (token, log, limite de taille)
    memory/              le domaine, sans SQL ni HTTP
      ingest.go          orchestration de l'écriture
      search.go          pipeline hybride et fusion
      units.go           construction des unités, troncature, UUIDv5
      access.go          règles de scope et d'ACL
      render.go          assemblage du context_block
    graph/               extraction, résolution d'entités, temporalité
    store/postgres/      requêtes, migrations SQL embarquées
    llm/                 client OpenAI-compatible
    jobs/                file Postgres et runner de workers
    config/              YAML plus surcharges d'environnement
  docs/design/
  docker-compose.yml
  Makefile
```

Le paquet `memory` ne dépend que d'interfaces, donc il se teste sans Postgres et sans
réseau. Les ports sont découpés par usage et non en une interface `Store` monolithique :
`ConversationRepo`, `MessageRepo`, `UnitRepo`, `GraphRepo`, `JobQueue`, `Embedder`,
`GraphExtractor`.

Dépendances runtime : `jackc/pgx/v5`, `pgvector/pgvector-go`,
`pgvector/pgvector-go/pgx`, `google/uuid` et `gopkg.in/yaml.v3`. Rien d'autre. Le routeur
est le `net/http` de la bibliothèque standard, qui sait router par méthode et motif depuis
Go 1.22.

Elles sont cinq et non quatre, et il faut le nommer parce qu'un invariant littéralement
faux ne protège plus rien. `pgvector/pgvector-go/pgx` est un module Go distinct, avec son
propre `go.mod` en amont, utilisé en code non-test dans `internal/store/postgres/pool.go`
pour l'enregistrement du type vectoriel auprès de `pgx`. C'est le sous-module frère d'une
dépendance déjà autorisée, épinglé à la même version, et il n'apporte aucun code tiers
nouveau au-delà de `github.com/x448/float16`, déjà indirect via `pgvector-go`. La
formulation à quatre était une omission, pas une dépendance introduite en fraude.

Convention de langue dans le code : identifiants, littéraux de chaîne et messages d'erreur
en anglais, suivant l'usage Go. Commentaires et documentation en français.

## 4. Modèle de données

Migrations SQL numérotées et embarquées par `go:embed`, appliquées par un runner intégré
au démarrage avec une table `schema_migrations`.

### 4.1 Extensions

```sql
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

### 4.2 Conversations

```sql
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
```

La colonne `next_sequence` est l'ajout par rapport à la spec source. L'attribution du
numéro de séquence se fait par `UPDATE conversations SET next_sequence = next_sequence + 1
WHERE conversation_id = $1 RETURNING next_sequence`, ce qui pose un verrou de ligne et
sérialise proprement les insertions d'une même conversation sans bloquer les autres.

### 4.3 Participants

```sql
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
```

### 4.4 Messages

```sql
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
```

Le cast explicite en `regconfig` est nécessaire : la forme à un seul argument de
`to_tsvector` est `STABLE` et Postgres refuse de l'utiliser dans une colonne générée.

Le `workspace_id` est dupliqué depuis la conversation pour éviter un join sur chaque
requête de recherche. Dénormalisation assumée, écrite une seule fois à l'insertion.

Postgres ne fait pas jouer l'unicité sur les `NULL`, donc plusieurs messages sans
`request_id` passent librement. C'est le comportement voulu.

### 4.5 Unités vectorielles

```sql
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
```

Ici `active = false` veut dire que l'unité a été invalidée par une édition ou une
suppression de l'un de ses messages.

Au démarrage, le service demande un embedding de test au modèle configuré et refuse de
démarrer si la dimension ne correspond pas à celle de la colonne, comme le demande la
section 6.4 de la spec source.

### 4.6 ACL explicites

```sql
CREATE TABLE memory_unit_acl (
    memory_unit_id UUID NOT NULL
        REFERENCES memory_units(memory_unit_id) ON DELETE CASCADE,
    principal_key  TEXT NOT NULL,
    permission     TEXT NOT NULL DEFAULT 'read',
    PRIMARY KEY (memory_unit_id, principal_key)
);
```

### 4.7 Entités du graphe

```sql
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
```

L'index unique partiel sur `canonical_key` est le mécanisme de déduplication des entités.

### 4.8 Relations bi-temporelles

```sql
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

-- Ajoutée par la migration 005: le classement de la stratégie graphe n'avait
-- aucun signal de pertinence par rapport à la question. Voir la section 8.2.
ALTER TABLE graph_relations ADD COLUMN embedding vector(768);

CREATE UNIQUE INDEX graph_relations_dedup_idx
    ON graph_relations (workspace_id, dedup_key);
CREATE INDEX graph_relations_source_idx
    ON graph_relations (source_entity_id) WHERE invalidated_at IS NULL;
CREATE INDEX graph_relations_target_idx
    ON graph_relations (target_entity_id) WHERE invalidated_at IS NULL;
```

Trois écarts par rapport à la section 6.7 de la spec source, tous nécessaires.

Le premier concerne les cibles littérales. La sortie de l'extracteur en section 13.2
produit un `target_literal` comme `"green"` qui n'a pas d'entité cible. Donc
`target_entity_id` devient nullable, `target_literal` apparaît, et un `CHECK` impose
qu'exactement une des deux soit renseignée.

Le deuxième est le modèle bi-temporel emprunté à Graphiti, qui remplace la colonne
`active`. Deux axes distincts au lieu d'un booléen ambigu. La validité du fait est portée
par `valid_from` et `valid_until`, où un `valid_until` à `NULL` veut dire toujours vrai.
Ce que le système croyait est porté par `ingested_at` et `invalidated_at`, où un
`invalidated_at` renseigné veut dire que la source a été supprimée ou éditée. Une
observation périmée reste donc crue avec un `valid_until` renseigné, ce qui préserve
l'historique demandé en section 13.4. `observed_at` reste distinct de `valid_from` parce
qu'un message du 9 septembre peut affirmer un fait vrai depuis juin.

Le troisième est `dedup_key`, qui rend la déduplication des relations une contrainte de
base et pas une politesse applicative. Elle est calculée par l'application et non par une
colonne générée, parce que la sérialisation d'un `timestamptz` en texte est `STABLE` et
non `IMMUTABLE`, donc inutilisable dans une colonne générée. Formule :

```text
dedup_key = source_entity_id + "|" + relation_type + "|"
          + (target_entity_id ou "lit:" + lower(target_literal)) + "|"
          + (valid_from en RFC3339 UTC, ou chaîne vide)
```

Cette formule au séparateur simple est celle retenue ici comme cible de conception ; ce
que l'implémentation calcule réellement diverge d'elle, pour une raison propre à
`relation_type` et `target_literal`. Voir la note de la section 7.3.

### 4.9 Sources des relations

```sql
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
```

### 4.10 File de jobs

```sql
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
```

Consommation en `FOR UPDATE SKIP LOCKED`, backoff exponentiel par `run_after`, passage en
`status = 'dead'` après cinq tentatives. La file de lettres mortes est une simple requête
sur ce statut.

### 4.11 Clients API

```sql
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
```

Le token présenté est haché en SHA-256 puis cherché sur l'index unique, donc la résolution
est une lecture par index et non un parcours. La comparaison finale du hash se fait en
temps constant.

`allowed_identities` est un tableau JSON de motifs. Un motif est soit une identité exacte
comme `agent:cuisine`, soit un préfixe terminé par une étoile comme `agent:*`. Le motif
`*` seul autorise toutes les identités du workspace.

Le `last_used_at` est mis à jour de façon paresseuse, au maximum une fois par minute et
par client, pour ne pas transformer chaque requête en écriture.

## 5. API

### 5.1 Enregistrement d'un message

```http
POST /v1/messages
Authorization: Bearer <token>
Idempotency-Key: evt-123
```

```json
{
  "workspace_id": "ws_1",
  "conversation_id": "conv_8453",
  "author_key": "user:paul",
  "role": "user",
  "content": "Je suis passé près de mes tomates, elles étaient encore vertes.",
  "created_at": "2026-09-09T16:00:00Z",
  "metadata": {},
  "consistency": "searchable"
}
```

`created_at` est optionnel et vaut l'heure de réception si absent. `consistency` vaut
`eventual` ou `searchable`, et sa valeur par défaut vient de la configuration.

Réponse :

```json
{
  "message_id": "0192f3c1-....",
  "conversation_id": "conv_8453",
  "sequence_number": 42,
  "status": "searchable",
  "memory_unit_ids": ["eb9bc69d-...."],
  "graph_status": "pending"
}
```

Les valeurs de `status` sont `stored` et `searchable`. En mode `searchable`, un échec de
l'embedder produit `stored` accompagné d'un champ `indexing_error`, parce que le message
est déjà durable et que répondre en erreur serait mensonger. Un job de rattrapage est posé.

La réponse porte aussi `embed_scheduled`, qui dit si un job `embed` est effectivement en
attente pour ce message. Sans lui, `stored` ne distingue pas une indexation seulement
différée d'une indexation perdue : les deux répondent la même chose. Il vaut `true` en
mode `eventual` pour un rôle indexé (le job est entré dans la transaction d'écriture,
étape 6 de la section 6.1) et après un échec d'embedder en mode `searchable` dont le
rattrapage a bien été posé, `false` sur un rejeu, sur un rôle non indexé et sur un
`searchable` réussi, où il n'y a rien à rattraper.

### 5.2 Création explicite d'une conversation

```http
POST /v1/conversations
```

```json
{
  "conversation_id": "conv_8453",
  "workspace_id": "ws_1",
  "scope": "participants",
  "participants": ["user:paul", "agent:cuisine"]
}
```

Optionnel. Utile pour un scope non par défaut ou des participants passifs.

### 5.3 Édition et suppression

```http
PATCH  /v1/messages/{message_id}
DELETE /v1/messages/{message_id}
```

### 5.4 Recherche

```http
POST /v1/memories/search
```

```json
{
  "workspace_id": "ws_1",
  "requester_key": "agent:cuisine",
  "conversation_id": "conv_9001",
  "query": "Tu te souviens de ce qu'on avait dit sur les tomates de Paul ?",
  "recent_messages": [
    { "author_key": "user:alice", "role": "user", "content": "Paul m'a parlé de son jardin." }
  ],
  "known_subjects": ["user:paul"],
  "strategies": ["dense", "lexical", "graph"],
  "candidate_limit": 20,
  "result_limit": 5,
  "token_budget": 1200,
  "exclude_message_ids": [],
  "include_context_block": true
}
```

Le champ `known_subjects` est un ajout par rapport à la section 10 de la spec source, qui
n'avait pas de quoi exprimer les "sujets explicitement connus" de sa section 11. S'il est
absent, le service détecte les sujets lui-même.

La réponse suit la section 10 de la spec source. `access_reason` vaut `workspace_scope`,
`conversation_participant` ou `explicit_acl`. `source_type` vaut `original_messages`. Le
bloc `debug` est piloté par la configuration et désactivé par défaut. Le `context_block`
n'est présent que si demandé.

### 5.5 Exploitation

```http
GET /health
GET /about
GET /debug/stats
```

`/health` vérifie Postgres par un ping et répond `200` ou `503`. Il ne sonde ni l'embedder
ni l'extracteur, parce qu'une panne de l'un ou de l'autre ne rend pas le service
indisponible : la recherche dégrade et l'ingestion continue en mode `eventual`. Sonder ces
dépendances ferait redémarrer un service parfaitement fonctionnel.

`/about` donne le nom du service, la version, le commit et la date de build, injectés par
`-ldflags`.

`/debug/stats` donne les compteurs et les profondeurs de file en JSON, plus l'état des
dépendances externes pour le diagnostic. Choix assumé à la place de Prometheus. Les
compteurs existant déjà, brancher `promhttp` plus tard resterait mécanique.

Ces trois routes ne demandent pas de token. `/health` et `/about` ne révèlent rien de
sensible. `/debug/stats` n'expose que des agrégats sans contenu de message ni identité.

## 6. Flux d'ingestion

### 6.1 La transaction

Une seule transaction fait tout le travail durable, dans cet ordre.

1. Création de la conversation en `ON CONFLICT DO NOTHING` si le `conversation_id` est
   inconnu, avec le scope par défaut de la configuration.
2. Ajout de l'auteur aux participants, même traitement.
3. Vérification de l'idempotence si une `Idempotency-Key` est présente. Si un message
   existe déjà pour ce couple conversation et clé, la transaction s'arrête et le service
   relit ce message et ses unités pour renvoyer la même réponse.
4. Attribution du numéro de séquence par `UPDATE ... RETURNING` sur la ligne conversation.
5. Insertion du message.
6. Insertion des jobs selon le mode de cohérence.

### 6.2 Idempotence

Deux verrous indépendants pour le critère d'acceptation 5. Le premier est
`UNIQUE (conversation_id, request_id)` sur les messages. Le second est le caractère
déterministe du `memory_unit_id`, qui fait qu'un rejeu allant jusqu'à l'indexation
retomberait de toute façon sur la même ligne.

### 6.3 Construction de l'unité vectorielle

Stratégie `contextualized_message`. Le message cible plus au maximum deux messages
précédents non supprimés, avec les rôles et auteurs conservés, précédés du préambule
`Conversation impliquant <participants>.`

Le filtre de rôle suit la configuration. Par défaut `user` et `assistant` sont indexés,
`system` ne l'est pas, `tool` est configurable.

Sur la limite de 400 tokens, pas de tokenizer embarqué. Le comptage se fait en
caractères avec un ratio documenté de quatre caractères par token, donc environ
1600 caractères. La troncature mange d'abord le contexte précédent et ne touche au message
principal qu'en dernier recours. Le contexte de nomic étant de 512 tokens, la marge est
réelle.

Le préfixe `search_document: ` est ajouté au moment de l'encodage et n'est pas stocké dans
`embedding_text`.

### 6.4 Les deux modes de cohérence

En `eventual`, la transaction pose un job `embed` et un job `graph_extract`, puis répond.

En `searchable`, seul le job `graph_extract` est posé, et l'embedding est calculé dans la
foulée avant de répondre. Un échec pose un job `embed` et dégrade le statut de la réponse,
comme décrit en 5.1.

### 6.5 Édition

L'édition met à jour le contenu et `edited_at`, puis désactive toutes les unités dont
l'intervalle `[start_sequence, end_sequence]` contient ce message. Pas seulement celles
qui l'ont pour ancre : les unités voisines qui le citaient en contexte deviendraient
mensongères. Un job de reconstruction est posé par ancre concernée, et un job
`graph_reeval` pour le message.

### 6.6 Suppression

Soft delete par `deleted_at`, désactivation des unités concernées dans la même transaction
pour satisfaire le critère 6, et pose d'un job `graph_reeval`. La recherche exclut de toute
façon les messages supprimés au moment de recharger les originaux, en ceinture et
bretelles.

## 7. Graphe

### 7.1 Entrée de l'extracteur

Le nouveau message, jusqu'à quatre messages de contexte, les identités de la conversation
et les entités candidates déjà connues du workspace. L'extracteur ne rescanne jamais la
conversation entière.

L'appel se fait en `/v1/chat/completions` avec un `response_format` de type `json_schema`.
Si le modèle ne respecte pas le schéma, le job échoue et repart en backoff, et le message
reste parfaitement consultable par le dense et le lexical.

### 7.2 Sortie attendue

Le JSON de la section 13.2 de la spec source, avec `entities` et `relations`, chaque
relation portant `observed_at`, `confidence` et ses `source_message_ids`.

### 7.3 Déduplication

Les entités se dédupliquent par `canonical_key` avec un `ON CONFLICT` sur l'index unique
partiel, qui fusionne les alias sans dupliquer la ligne.

Quand l'extracteur ne sait pas résoudre une entité, il produit une entité
`resolved = false` avec une clé portant la conversation d'origine, par exemple
`unresolved-person:paul-conv-8453`. Le service ne fusionne jamais deux entités de sa propre
initiative, conformément à la règle 13.3 de la spec source. La consolidation reste hors
périmètre.

Les relations se dédupliquent par `dedup_key`. Sur conflit, le service n'insère pas de
doublon et se contente d'ajouter la nouvelle source dans `graph_relation_sources` en
gardant la confiance la plus élevée.

`dedup_key` n'est pas calculée avec le séparateur simple de la section 4.8. Chaque
composante y est encodée par sa longueur plutôt que jointe par `"|"`, parce que
`relation_type` et `target_literal` sortent d'un modèle de langage et peuvent contenir
n'importe quel caractère, ce séparateur compris : une relation dont le type contiendrait
littéralement un `|` casserait la formule simple en fusionnant deux relations distinctes
sous une même clé, ou en séparant une seule relation en deux. Voir
`internal/graph.DedupKey` et le commentaire qui l'accompagne.

### 7.4 Temporalité

Une liste configurable de types de relations à valeur unique, contenant `a_pour_etat`
par défaut. Ce nom n'est pas arbitraire : il doit figurer dans le vocabulaire de prédicats
que le prompt d'extraction impose au modèle, sans quoi le chaînage décrit ci-dessous ne se
déclenche jamais. C'est exactement ce qui s'est produit jusqu'à la première évaluation
contre un extracteur réel : le défaut était `has_observed_state`, un identifiant anglais,
pendant que le prompt était en français et que le modèle produisait `a_pour_etat`. Aucune
erreur ne se déclenchait, la fonctionnalité était simplement morte. Un test garde
désormais l'accord entre les deux. Quand une nouvelle observation arrive pour un couple source et type figurant
dans cette liste, avec un `observed_at` postérieur, l'observation précédente se ferme par
un `valid_until` égal à l'`observed_at` de la nouvelle. Rien n'est jamais supprimé.

Ce recalcul est complet, pas incrémental : il repart à chaque fois de la liste entière des
observations non invalidées du couple, triées par `observed_at`, et réécrit leur
`valid_until` en totalité plutôt que de ne toucher que les deux observations les plus
récentes. C'est ce qui évite un trou dans la chaîne quand c'est l'observation la plus
récente qui disparaît (section 7.5), au prix de retraverser tout l'historique du couple à
chaque écriture ou réévaluation qui le touche. Et cette autorité sur `valid_until` ne
s'exerce que pour les types de relation listés dans `single_valued_relations` : un type qui
n'y figure pas voit son `valid_until` posé une fois par l'extracteur et jamais recalculé
ensuite, quel que soit le nombre d'observations ultérieures pour le même couple.

### 7.5 Réévaluation

Un job `graph_reeval` sur un message regarde les relations qu'il sourçait. Si toutes leurs
sources sont supprimées, la relation prend un `invalidated_at`. Si au moins une source
survit, la relation reste crue.

Ensuite, pour chaque couple source et type touché, le service recalcule la chaîne complète
des fenêtres de validité en repartant des relations non invalidées triées par
`observed_at`. C'est ce qui évite de laisser un trou quand c'est l'observation la plus
récente qui disparaît.

## 8. Flux de recherche

### 8.1 Construction de la requête

Le texte suit la section 11 de la spec source : le demandeur, les sujets connus, jusqu'à
quatre messages récents et la question courante. Préfixe `search_query: ` ajouté à
l'encodage, jamais stocké et jamais traduit.

### 8.2 Les trois stratégies

Elles partent en parallèle, chacune dans sa goroutine, et chacune applique l'ACL dans son
propre SQL.

Pas d'`errgroup` ici, volontairement. `errgroup` annule tout au premier échec alors qu'on
veut l'inverse. Un `WaitGroup` avec collecte des erreurs par stratégie fait qu'une panne
d'Ollama laisse le lexical et le graphe répondre, et qu'un graphe cassé laisse le dense et
le lexical répondre. C'est le critère 10, et ça le dépasse puisque ça couvre aussi la panne
de l'embedder.

**Dense.** Encodage de la requête puis `ORDER BY embedding <=> $1` sur les unités actives,
limité à `dense_top_k`.

**Lexical.** `websearch_to_tsquery('french', ...)` sur la colonne `tsv` des messages,
classé par `ts_rank_cd`, limité à `lexical_top_k`. Chercher dans les messages et non dans
les unités évite que le préambule fausse le classement, et rend utilisables les messages
pas encore vectorisés, ce qui est le repli demandé en section 15 de la spec source.

**Graphe.** Les entités graines viennent de trois sources : les `known_subjects` fournis,
les participants de la conversation courante, et une détection par comparaison des tokens
de la requête contre `canonical_key`, `display_name` et `aliases`, avec `pg_trgm` pour
tolérer les variantes. Une CTE récursive parcourt ensuite les relations non invalidées sur
un ou deux sauts, puis remonte aux `message_id` par `graph_relation_sources`. Classement par distance croissante, puis confiance décroissante, puis `observed_at`
décroissant.

Cet ordre a été corrigé sur deux points, mesurés sur le corpus étendu, et les deux
corrections sont des écarts assumés à ce que cette section décrit.

Le premier est que trier d'abord par nombre de sauts, avec une limite de lignes, condamne
les faits du dernier saut dès qu'une entité graine est un peu connectée. Mesuré : pour une
question à deux sauts, les treize faits rendus étaient tous à un saut de la graine, et les
relations du second saut, présentes en base et lisibles, n'arrivaient jamais, alors que ce
sont exactement celles qui rendent la question répondable. L'ordre entrelace donc les
profondeurs au lieu de les empiler : le meilleur fait de chaque profondeur, puis le
deuxième de chaque.

Le second est plus profond. Ni le nombre de sauts, ni la confiance, ni la récence ne
mesurent la pertinence par rapport à la question posée. Mesuré : trois questions sans
aucun rapport entre elles recevaient exactement les mêmes quatorze faits, le voisinage de
l'entité graine, parce que rien ne permettait de préférer celui qui répond. Chaque relation
porte donc désormais la représentation vectorielle de son fait rendu en phrase, calculée à
l'écriture par le même modèle que les unités de mémoire, et le classement passe d'abord par
la distance cosinus à la question. La colonne est nullable et l'ordre retombe sur le
précédent quand elle est vide, une panne de l'embedder dégradant le classement sans perdre
la relation.

### 8.3 L'ACL, écrite une seule fois

C'est le point le plus verrouillé du design, parce que c'est le critère 4. Toute requête de
candidats commence par la même CTE.

```sql
WITH readable AS (
  SELECT c.conversation_id
  FROM conversations c
  WHERE c.workspace_id = $1
    AND c.deleted_at IS NULL
    AND (
      c.scope = 'workspace'
      OR EXISTS (
        SELECT 1 FROM conversation_participants p
        WHERE p.conversation_id = c.conversation_id
          AND p.participant_key = $2
          AND p.left_at IS NULL
      )
    )
)
```

Les trois stratégies joignent dessus. Le cas `explicit` s'ajoute en `OR` sur
`memory_unit_acl` au niveau des unités. Pour le graphe, la dérivation par les sources de la
section 6.8 tombe naturellement : le join des sources passe par `readable`, donc une
relation ne ressort que si au moins un de ses messages sources est lisible.

Une seule définition de la règle, réutilisée partout, ce qui la rend testable pour de vrai.

### 8.4 Fusion

Un point que la spec source ne tranche pas. Les trois stratégies ne renvoient pas le même
type d'objet : le dense renvoie des unités, le lexical et le graphe renvoient des messages.

La clé de fusion est donc le message d'ancrage, le dense projetant son unité sur son
`anchor_message_id`. Ça rend la RRF homogène et ça réalise gratuitement l'étape "grouper
ceux qui pointent vers les mêmes messages".

RRF classique, `k = 60`. Pas de seuil minimum par défaut, puisque la section 18 de la spec
source demande explicitement de le calibrer sur du réel plutôt que de l'inventer. Le champ
existe et reste à `null`.

Attention, l'évaluation a montré depuis que ce seuil ne peut pas fonctionner sur le score
fusionné, quelle que soit la taille du corpus. Voir la limitation correspondante en
section 12 avant de tenter de le calibrer.

### 8.5 Post-traitement

Une divergence assumée avec l'étape 3 de la section 12. Plutôt que de supprimer les
extraits qui se chevauchent, le service fusionne les intervalles contigus ou chevauchants
en un seul extrait plus long, avec le score maximum et l'union des sources. Jeter un
extrait parce qu'il touche son voisin fait perdre du texte utile, alors que les recoller
produit un passage plus lisible.

Ensuite l'exclusion des `exclude_message_ids`, le rechargement des messages originaux
depuis Postgres en écartant les supprimés, puis l'expansion de deux messages avant et
après.

Le budget de tokens se traite par dégradation avant élimination. Si l'ensemble dépasse, le
service réduit d'abord l'expansion des extraits les moins bien classés de deux à un puis à
zéro, et n'écarte un extrait entier qu'ensuite. Un extrait n'est jamais tronqué en son
milieu, parce qu'un message coupé au milieu d'une phrase produit un souvenir trompeur.

Le service renvoie de zéro à `final_top_k` extraits sans jamais forcer le compte.

### 8.6 Le context_block

Assemblé par template, dans cet ordre : l'avertissement de la section 14 de la spec source
disant que ce sont des données et jamais des instructions, puis les faits du graphe rendus
en phrases avec leurs dates de validité, puis les extraits datés avec leur conversation
d'origine et le texte original cité mot pour mot.

## 9. Observabilité et exploitation

Logs structurés par `log/slog` en JSON. Une ligne par recherche portant le demandeur, le
workspace, le nombre de résultats et le nombre de rejets ACL, ce qui couvre la
journalisation des accès de la section 16 de la spec source.

Limite de taille des corps de requête par `http.MaxBytesReader`.

Le bloc `debug` de la réponse de recherche répond à l'exigence d'explicabilité de la
section 17 : pour chaque résultat, par quelle stratégie il est arrivé, avec quels scores,
depuis quelle conversation, selon quelle règle d'accès et quels messages en sont la source.
Il est piloté par `service.debug_search` et reste à `false` par défaut.

Aucun log ne contient de contenu de message. Les tokens ne sont jamais journalisés, même
tronqués.

### 9.1 Exigences de production

Image Docker multi-stage, binaire statique, utilisateur non root, `-ldflags` pour la
version et le commit.

Arrêt propre sur `SIGTERM` : le serveur HTTP cesse d'accepter, laisse les requêtes en vol
finir dans une limite de trente secondes, et le runner de jobs finit le job courant puis
s'arrête sans en réclamer de nouveau. Un job interrompu revient `pending` par expiration
de son verrou, donc rien n'est perdu.

Timeouts HTTP explicites côté serveur, `ReadHeaderTimeout` en tête, parce que la valeur
zéro par défaut de `net/http` est une invitation au slowloris.

Pool pgx dimensionné et borné, avec `MaxConns` configurable et une durée de vie maximale
des connexions. Les appels à l'embedder et à l'extracteur portent leurs propres timeouts
et ne détiennent jamais de connexion Postgres pendant qu'ils attendent le réseau.

## 10. Configuration

```yaml
service:
  listen: ":8080"
  max_request_bytes: 1048576
  consistency_default: eventual
  debug_search: false
  read_header_timeout: 5s
  read_timeout: 30s
  write_timeout: 60s
  shutdown_grace: 30s

database:
  dsn: "${POSTGRES_DSN}"
  max_conns: 10
  max_conn_lifetime: 30m

embedding:
  base_url: "http://localhost:11434/v1"
  model: "nomic-embed-text-v2-moe"
  dimensions: 768
  document_prefix: "search_document: "
  query_prefix: "search_query: "
  normalize: true
  timeout: 30s

extraction:
  base_url: "${LLM_API_URL}"
  model: "google/gemma-4-31B-it"
  api_key: "${LLM_API_KEY}"
  timeout: 120s

indexing:
  strategy: contextualized_message
  previous_messages: 2
  max_chars: 1600
  chars_per_token: 4
  index_user_messages: true
  index_agent_messages: true
  index_system_messages: false
  index_tool_messages: false
  version: 1

retrieval:
  dense_top_k: 20
  lexical_top_k: 20
  graph_top_k: 20
  final_top_k: 5
  max_memory_tokens: 1200
  expand_before: 2
  expand_after: 2
  rrf_k: 60
  minimum_score: null

graph:
  enabled: true
  context_messages: 4
  max_hops: 2
  single_valued_relations: ["a_pour_etat"]

access:
  default_scope: participants

jobs:
  workers: 2
  retry_limit: 5
  poll_interval: 1s
```

## 11. Tests

En TDD.

Les tests unitaires purs ne touchent ni Postgres ni le réseau : construction et troncature
des unités, déterminisme de l'UUIDv5, calcul de la `dedup_key`, RRF, fusion des
intervalles, dégradation du budget, rendu du `context_block`, chaînage des `valid_until`
lors d'un changement d'état.

Les tests d'intégration tournent sur un vrai Postgres lancé par `docker-compose`, adressé
par `POSTGRES_TEST_DSN`, avec un skip propre si la variable est absente. L'embedder et
l'extracteur y sont remplacés par des faux déterministes, le faux embedder dérivant son
vecteur d'un hash du texte. Ça ne teste pas la sémantique mais ça valide toute la plomberie
de façon reproductible.

Le harnais principal est un test nommé par critère d'acceptation de la section 20 de la
spec source, portant son numéro.

1. Un message enregistré en mode `searchable` est retrouvable au tour suivant.
2. Une paraphrase retrouve un ancien message pertinent.
3. Une recherche exacte retrouve les noms et termes rares.
4. Un agent non autorisé ne reçoit aucun extrait de la conversation.
5. Le rejeu du même événement ne crée pas de doublon.
6. La suppression d'un message le retire immédiatement des résultats.
7. Chaque résultat retourne ses `message_id` sources.
8. Le contexte injecté respecte le budget de tokens.
9. Aucun résumé LLM n'est nécessaire sur le chemin critique.
10. Le système continue de fonctionner si la couche graphe est indisponible.

Trois tests s'ajoutent pour la couche d'authentification, qui ne figurait pas dans les
critères de la spec source. Un token révoqué est refusé. Un token valide qui réclame un
autre `workspace_id` que le sien est refusé, et ce refus est vérifié à l'ingestion comme à
la recherche. Un token dont les `allowed_identities` ne couvrent pas l'identité déclarée
est refusé, y compris quand le motif est un préfixe.

Les critères 2 et 3 ne veulent rien dire avec un faux embedder. Ils passent par une
commande d'évaluation séparée qui tape le vrai Ollama sur un petit jeu de conversations
annotées et calcule un Recall@5. Hors tests automatiques. Elle devait aussi servir à calibrer
`minimum_score`, et c'est elle qui a établi que ce seuil porte sur la mauvaise grandeur :
voir la limitation en section 12.

### 8.7 Réordonnancement, facultatif et mesuré

La spec source ne prévoyait pas de réordonnancement, et le critère 9 interdit tout appel de
modèle sur le chemin d'une requête de recherche. Le service en a néanmoins un, désactivé par
défaut, parce que la mesure sur le corpus étendu a montré que c'est le seul moyen de gagner
les deux requêtes dont la réponse se trouve entre le rang six et le rang dix.

Aucun signal déjà disponible ne les remonte : sur l'une, l'embedder classe réellement cinq
distracteurs devant la bonne réponse, et l'ordre final suit déjà exactement l'ordre dense ;
sur l'autre, la bonne réponse est plus récente mais son score est inférieur de 0,08, et un
biais de récence assez fort pour renverser cet écart casserait toute question portant sur le
passé.

Le réordonnanceur reçoit un vivier plus large que `final_top_k`, puisque sans marge il n'y
a rien à réordonner, et son ordre décide des extraits rendus. Une panne fait retomber sur
l'ordre de la fusion, jamais échouer la recherche : c'est la posture du critère 10 appliquée
à un composant que le critère 9 interdit de rendre nécessaire. Chiffres et latence dans
`docs/evals/2026-09-10-recall-corpus-etendu.md`.

## 12. Limitations assumées

L'authentification vérifie le workspace et l'identité déclarée, mais un token reste un
secret partagé sans expiration. La rotation est manuelle via `cinnabar keys create` puis
`cinnabar keys revoke`. Il n'y a ni expiration automatique ni périmètre par route.

Les routes de santé se limitent à `/health` et `/about`. Un déploiement Kubernetes qui
distingue les sondes pourra demander des routes séparées (`/health/live`,
`/health/ready`, `/health/started`) avec leurs codes de retour.

L'accès est évalué sur l'appartenance actuelle aux participants et non sur l'appartenance
au moment du message. Comme les participants ne font que s'accumuler, personne ne quitte
jamais une conversation et la question ne se pose pas encore.

Le comptage de tokens est une approximation par caractères, sans tokenizer.

La consolidation des entités non résolues n'est pas faite. Deux personnes portant le même
prénom restent deux entités distinctes, ce qui est le comportement voulu mais laisse des
doublons à traiter plus tard.

Les entités du graphe sont portées par le workspace et non par la conversation, et la
stratégie graphe est le premier chemin de lecture qui les fait remonter. Trois conséquences
mesurées lors de la revue de la tâche 8, toutes de faible portée mais réelles. Le
`display_name` d'une entité est celui de sa première observation et n'est jamais écrasé :
si cette première observation vient d'une conversation qu'un demandeur ne peut pas lire,
ce nom lui parvient tout de même dans le `context_block`, porté par un fait dont la source
est ailleurs et parfaitement lisible. La `confidence`, fusionnée par `GREATEST`, et
l'`observed_at`, gardé de la première observation, se déplacent de la même façon. Enfin la
traversée n'applique aucun filtre d'accès, délibérément, puisque l'accès se dérive des
messages sources : un demandeur peut donc déduire que deux entités sont reliées en deux
sauts alors que la seule arête qui les relie n'est attestée que dans une conversation
qu'il ne lit pas. Dans ces trois cas, aucun message, aucun extrait et aucun identifiant de
message ne franchit la frontière : ce qui franchit est un nom d'entité et une distance.

Cette dernière phrase a d'abord été écrite comme une garantie générale sur la stratégie
graphe, et la revue finale de branche a montré qu'elle était fausse formulée ainsi. Un
`target_literal` est un fragment que le modèle a levé du texte d'un message source : ce
n'est ni un nom d'entité ni une distance. Or la fenêtre que voit l'extracteur
(`graph.context_messages`, 4 par défaut) est deux fois plus large que celle qu'une unité de
mémoire couvre (`indexing.previous_messages`, 2 par défaut), et `filterSources` admet
délibérément toute la fenêtre d'extraction comme source. Une relation extraite en traitant
M4 pouvait donc être sourcée par M0 et ressortir par une ACL explicite sur l'unité qui
couvre `[M2, M4]`, en emportant un littéral énoncé dans M0 seulement, hors de la fenêtre
octroyée. Le bornage est désormais dans le SQL : un fait dont la raison d'accès est
`explicit_acl` ne ressort que si **toutes** ses sources tombent dans le
`[start_sequence, end_sequence]` de l'unité octroyée, une source d'une autre conversation
comptant comme hors fenêtre. La règle « au moins une source lisible » reste celle des
accès dérivés du scope, où toute la conversation est lisible par construction.

Le résiduel après ce bornage, dit platement : à l'intérieur d'une même conversation, le
modèle peut toujours attribuer un littéral à un message source qui ne le contient pas,
puisque `filterSources` admet toute la fenêtre d'extraction et pas seulement le message
ancre. Un fait rendu peut donc citer M2 comme source d'un littéral énoncé en M0 de la même
conversation. Pour un accès dérivé du scope c'est sans conséquence : la conversation
entière est lisible, donc le demandeur pouvait de toute façon lire M0. Pour un accès par
ACL explicite, qui est le seul cas où ça ne l'était pas, le bornage ci-dessus ferme la
question, au prix de faits qui disparaissent quand une de leurs sources sort de l'unité
octroyée. Ce qui n'est pas garanti, et ne peut pas l'être à ce niveau, c'est l'exactitude
de l'attribution d'un littéral à celui de ses messages sources qui l'énonce vraiment : ça
se joue dans la qualité de l'extraction, pas dans la règle d'accès.

Le `valid_until` d'un fait suivait le même chemin, pour une raison différente, et la revue
finale l'a également fermé. Le recalcul de chaîne de la section 7.4 porte sur toutes les
observations non invalidées d'un couple, sans filtre de conversation ni d'accès, ce qui est
correct puisque c'est de la maintenance. Mais l'observation qui supplante est une ligne
*différente* : quand sa seule source est dans une conversation que le demandeur ne peut pas
lire, le SQL refuse bien cette ligne tout en laissant passer sa date sur le fait lisible.
Le demandeur apprenait donc qu'à telle date exacte, dans une conversation fermée, quelque
chose avait supplanté ce fait, et la date partait en français dans le `context_block`. Ce
n'est pas le canal accepté juste au-dessus pour `observed_at` et `confidence` : ces deux-là
ne bougent que quand le même triplet est affirmé des deux côtés, donc sur la même ligne
dédupliquée, et rien de nouveau ne franchit. La lecture dérive maintenant `valid_until` :
il est mis à `NULL` seulement lorsqu'une observation du couple (entité source, type) porte
cet instant comme `observed_at` **sans être lisible** par ce demandeur. C'est exactement ce
que le demandeur croirait des seuls messages qu'il peut lire.

La distinction se fait sans colonne d'origine, et il vaut de dire pourquoi elle fonctionne.
Un `valid_until` calculé par la chaîne vaut toujours, par construction, l'`observed_at`
d'une autre observation du couple : le recalcul de la section 7.4 ferme chaque observation
sur l'instant de la suivante, jamais sur autre chose. Une date posée par l'extracteur, pour
un type qui n'est pas dans `single_valued_relations` et que la chaîne ne recalcule donc
jamais, n'a aucune raison de coïncider avec l'une d'elles. « Aucune observation du couple
ne porte cet instant » identifie donc la seconde, et elle reste rendue : elle est affirmée
par un message que le demandeur peut lire, et la masquer perdrait de l'information sans
rien protéger. La première formulation de cette règle masquait les deux, ce qui contredisait
son propre motif ; c'était la formulation qui était trop large.

Le test d'existence sur les frères de couple ne filtre pas les observations invalidées, et
c'est délibéré. Une première version le faisait, et la prémisse ci-dessus, écrite comme un
« toujours », ne tenait alors que tant que la chaîne du couple était à jour : dès qu'une
observation est invalidée sans que son couple soit recalculé, la ligne fermée garde un
`valid_until` que plus aucune observation visible ne justifie, et la date repassait. Or une
relation invalidée a toutes ses sources mortes, donc elle n'est lisible par personne :
rendre la date qu'elle a posée revient à divulguer celle d'une observation qui n'existe plus
pour ce demandeur. Ne pas filtrer, donc masquer, est le choix conservateur et le seul
cohérent avec le motif de la règle.

**Lacune de maintenance connue, et c'est là qu'est la vraie cause.** Ce `valid_until`
périmé ne devrait pas rester sur la ligne : un recalcul de chaîne l'aurait effacé. Il
survit parce que le recalcul ne s'applique qu'aux types listés dans
`single_valued_relations` (section 7.4). Retirer un type de cette liste **gèle donc les
chaînes qu'il avait construites** : leurs `valid_until` restent en base, aucune écriture ni
aucune réévaluation ultérieure ne les rouvre, et ils survivent à l'invalidation de
l'observation qui les avait posés. Rien dans le service ne balaie ces chaînes gelées.
Le masquage à la lecture décrit ci-dessus n'est que le filet ; la correction propre serait
un recalcul déclenché au changement de configuration, ou une colonne qui enregistre quel
type a construit la chaîne. Aucun des deux n'est fait.

Trois autres prix, écrits plutôt que laissés à découvrir. Le cas de coïncidence : si une
date posée par l'extracteur tombe par hasard sur l'`observed_at` d'une autre observation du
même couple qui n'est pas lisible — invalidée comprise, depuis le choix ci-dessus — elle est
masquée, la lecture ne distinguant pas les deux origines autrement que par cette
coïncidence. Le cas de la cible : la traversée admet une relation dès que son entité source
**ou** son entité cible est atteinte, donc un fait peut entrer par sa cible alors que son
entité source ne l'est pas ; ses frères de couple à cible littérale n'entrent alors pas dans
la traversée, et un `valid_until` pourtant entièrement lisible se trouve masqué. Enfin, sur
le bornage `explicit_acl` de la section précédente : le prédicat s'évalue ligne par ligne,
donc sur une seule unité à la fois, et un demandeur détenant deux octrois dont les fenêtres
couvrent à elles deux toutes les sources d'un fait ne l'obtient quand même pas. Les trois
vont dans le sens du secret, ce qui est le bon sens pour ces gardes ; les deux premiers se
paient en information perdue sur un fait par ailleurs rendu, le troisième en faits retenus.
Une colonne d'origine sur `graph_relations` fermerait le premier ; c'est un changement de
schéma, sans rapport avec une frontière de lecture.

Dernière asymétrie du bornage `explicit_acl`, à écrire parce qu'elle a l'air d'un oubli et
n'en est pas : le prédicat ne filtre pas le `deleted_at` des messages sources qu'il examine,
alors que la requête qui l'entoure en porte un juste à côté. Une source hors fenêtre qui a
été supprimée continue donc de retenir le fait, définitivement, puisque rien ne retire jamais
de ligne de `graph_relation_sources`. C'est voulu : le littéral que le modèle a levé de ce
message survit dans `graph_relations` après sa suppression, donc le refuser à un demandeur
qui n'a jamais eu accès qu'à la fenêtre octroyée est exactement ce que ce prédicat existe
pour faire. Ajouter le filtre rendrait le fait au moment de la suppression du message,
c'est-à-dire quand il devient le plus difficile à justifier. Un test l'épingle, pour que la
« correction » de cette asymétrie échoue au lieu d'être livrée.

La décomposition des sujets connus ignore les namespaces d'entités, par construction. Un
sujet est une clé d'identité comme `user:paul` alors qu'une entité porte une clé canonique
comme `person:paul` : les deux ne s'égalent jamais, donc le service compare aussi les
parties locales, ce qui jette le préfixe de type des deux côtés. Un `person:paul` fourni
explicitement sème donc aussi un `dog:paul` du même workspace. C'est un problème de
précision et de bruit, pas d'accès : l'ensemble des relations considérées grossit,
l'ensemble des messages rendus ne bouge pas. Le nombre de sujets est borné à 32 par la
couche HTTP pour que ce bruit ne soit pas amplifiable depuis l'extérieur.

La détection des entités candidates qui alimentent le prompt d'extraction ne peut pas
utiliser d'index trigramme. Elle cherche un nom court à l'intérieur d'un message long, et
c'est la direction que `pg_trgm` n'indexe pas : son index accélère la recherche d'une
aiguille dans une colonne longue, pas l'inverse. Le parcours est borné au workspace par
`graph_entities_workspace_idx`, ce qui suffit tant que les entités sont réparties entre
plusieurs workspaces, et ne sert à rien dans un déploiement mono-tenant, où le workspace
contient par construction toutes les lignes. Le coût reste supportable parce que cette
requête vit sur le chemin d'un worker d'extraction, une fois par message ingéré, jamais sur
le chemin d'une recherche.

La clé canonique d'une entité n'est pas normalisée Unicode. Un nom écrit en NFC et le même
nom écrit en NFD produisent deux clés, donc deux entités que le service ne fusionnera
jamais de lui-même, comme n'importe quelle autre paire d'entités. La bibliothèque standard
n'offre pas de normalisation Unicode et l'ajouter demanderait `golang.org/x/text`, exclu
par la contrainte de dépendances. Ce qui est garanti, en revanche, c'est qu'aucune des deux
formes ne perd son accent au passage : les marques diacritiques combinantes sont conservées
par le calcul du slug, sans quoi la forme décomposée de « café » serait devenue « cafe »
silencieusement. En pratique les modèles d'extraction produisent du NFC, donc le cas reste
théorique tant que les noms viennent d'eux.

Le scope `private` est présent dans le `CHECK` et dans le modèle, mais son comportement
est identique à `explicit` dans cette version puisque les deux se résolvent par la table
`memory_unit_acl`.

La question que la spec source laissait ouverte sur `memory_unit_acl`, celle de savoir qui
a le droit de partager un souvenir dont il n'est pas l'auteur, a fini par être tranchée :
partager exige d'incarner un participant de la conversation d'origine, ou que celle-ci soit
en scope `workspace`. C'est la règle `canShare`, la même que pour supprimer une
conversation entière. Les routes `POST` et `GET /v1/memories/{id}/acl` écrivent et lisent
donc la table, et les scopes `explicit` et `private` sont utilisables sans SQL direct.

La suppression d'une conversation entière a sa route, `DELETE /v1/conversations/{id}`,
sous la même règle d'autorisation. Elle masque la conversation et désactive toutes ses
unités dans une seule transaction ; les messages restent en base comme source de vérité.
La même transaction met en file un `graph_reeval` par message de la conversation qui source
réellement une relation, et aucun pour une conversation qui n'a rien alimenté. Sans ces
jobs, les relations sourcées uniquement par cette conversation ne prenaient jamais
d'`invalidated_at` et aucun balayage ne repassait, `Reevaluate` n'étant appelée que par un
job : l'invariant de la section 7.5 était violé pour tout ce chemin, et un `valid_until`
posé par une de ces relations restait en place.

Ce qui reste vrai, en revanche, c'est qu'une ACL explicite ne donne accès qu'à l'unité
qu'elle nomme. L'expansion de contexte est bornée à la fenêtre de l'unité partagée pour un
candidat obtenu par cette voie, et la liste des participants n'est pas rendue : un
bénéficiaire qui ne peut pas lire la conversation n'a pas à en apprendre la composition.
Une ACL explicite n'est donc pas un raccourci vers la conversation entière.

Les codes de refus suivent une convention que la spec ne fixait pas et qu'il
faut donc énoncer ici, parce que deux routes voisines pourraient sinon
diverger. Une ressource qui existe dans un autre workspace rend `404`, comme
si elle n'existait pas : c'est le cas de `PATCH`/`DELETE /v1/messages/{id}`,
de `DELETE /v1/conversations/{id}`, des routes ACL et de
`POST /v1/conversations` sur une conversation déjà déclarée ailleurs. Un `403`
y confirmerait l'existence d'une ressource dans le workspace d'un voisin, pour
un identifiant que l'appelant ne fait que deviner. Sur une route qui crée la
ressource, en revanche, il n'y a rien à confirmer ni à nier : un
`conversation_id` déjà pris dans un autre workspace rend `400` avec un message
qui ne distingue pas les cas, parce que l'identifiant est simplement
inutilisable pour cet appelant. C'est le comportement de
`POST /v1/messages`. Le `403` reste réservé aux refus qui portent sur
l'identité ou le périmètre du token à l'intérieur du workspace de l'appelant,
là où rien de nouveau ne se voit divulgué.

`retrieval.minimum_score`, tel que spécifié en 8.4, s'applique au score fusionné par RRF.
La commande d'évaluation de la tâche 17 a montré que ce score n'encode qu'un rang au sein
d'une stratégie (`1 / (rrf_k + rang)`), jamais une distance sémantique : deux candidats de
rang identique reçoivent le même score qu'ils soient pertinents ou non, et rien dans la
taille du corpus ne change ça, seulement l'étalement des rangs. Un seuil posé sur ce score
ne peut donc jamais servir de plancher de pertinence : trop bas il ne filtre rien, assez
haut pour isoler les candidats retrouvés à la fois par dense et par lexical il élimine
aussi les correspondances purement sémantiques, exactement celles que mesure le critère 2.
Le service sait désormais ne rien rendre, par deux dispositifs mesurés sur le corpus étendu
et décrits dans `docs/evals/2026-09-10-recall-corpus-etendu.md`. Le premier est
un plancher sur le score dense brut. Le second, plus intéressant, écarte les candidats du
dense en bloc quand son meilleur candidat est à la fois lointain et noyé dans un voisinage
plat : la mesure a établi que ni la similarité absolue ni la marge au voisinage ne séparent
seules une question sans réponse d'une question qui en a une, et qu'une question qui a une
réponse présente toujours au moins l'un des deux signes. La conjonction est donc ce qui
rend la règle sûre, et elle ferme trois des quatre questions sans réponse du corpus sans
coûter une seule vraie réponse.

Ni l'un ni l'autre ne touche au lexical ni au graphe, qui savent déjà se taire quand ils
n'ont rien trouvé. Une mesure connexe le confirme par la négative : assembler les termes de
la requête lexicale par OU plutôt que par ET fait passer le rappel global de 78 % à 61 %,
parce que la stratégie se met alors à répondre quelque chose à tout, ce qui détruit à la
fois la fusion et le signal de non-réponse.

Un vrai plancher de pertinence porte sur le score brut de la stratégie dense avant fusion,
où la distance cosinus garde son sens. Il existe désormais, sous le nom
`retrieval.minimum_dense_score`, et la mesure a corrigé la supposition faite ici sur deux
points. D'abord les distributions du score dense se chevauchent elles aussi, donc un
plancher par résultat ne sépare pas le pertinent du non pertinent ; ce qu'il sépare, c'est
une question à laquelle rien ne répond d'une question qui a une réponse, ce qui est une
décision au niveau de la requête. Ensuite le plancher ne doit filtrer **que** les candidats
du dense : une correspondance lexicale et un fait du graphe sont des preuves de pertinence
par elles-mêmes, et le corpus contient une requête qui réussit par le lexical avec un score
dense plus bas que celui de deux requêtes sans réponse. Filtrer sur le score fusionné, ou
filtrer toutes les stratégies au même seuil, perd cette requête pour fermer les deux
autres. Détail et distributions dans
`docs/evals/2026-09-10-recall-graphe.md`. Voir
`docs/evals/2026-09-10-recall-baseline.md` pour le détail et les chiffres.

`graph_top_k` et la limite bornant `SearchResponse.GraphFacts` (section 8.6) portent sur le
nombre de lignes `graph_relation_sources` que la traversée retient, pas sur le nombre de
faits distincts qui en ressortent : une même relation sourcée par plusieurs messages compte
plusieurs fois côté lignes avant de se réduire à un seul fait au rendu. La limite réellement
perçue par l'appelant sur le nombre de faits est donc plus basse, et de façon variable, que
la valeur configurée. Plus largement, le rappel de la stratégie graphe dépend entièrement de
la qualité de l'extraction en amont : une entité que le modèle n'a pas su relier, une
relation qu'il n'a pas extraite, ou un `relation_type` qu'il a formulé différemment d'une
fois sur l'autre restent invisibles à la traversée sans qu'aucune erreur ne se déclenche
nulle part. Aucun test automatisé ne mesure cette qualité d'extraction, et l'évaluation
contre le vrai extracteur a montré que la réserve n'était pas théorique : avec le prompt
d'origine, le modèle extrayait les propriétés d'une entité et négligeait les relations
entre deux entités nommées dans une même phrase, si bien qu'une requête à deux sauts
échouait faute de sa première arête alors que la traversée fonctionnait. Le prompt a été
réécrit sur ce point, avec un vocabulaire de prédicats semi-fermé et un exemple, et la
même requête réussit désormais de façon stable. Voir
`docs/evals/2026-09-10-recall-graphe.md` pour les chiffres et le diagnostic.

Ce que cette mesure dit du reste de la couche mérite d'être retenu : le prompt et le schéma
sont la surface qui décide du rappel, et ils ne sont couverts par aucun test unitaire
possible. Toute évolution du graphe devrait commencer par une mesure sur ce harnais.

La même évaluation a d'abord semblé mettre au jour un défaut de la fusion, et la suite de
la mesure a montré qu'il n'en était pas un. Avec le prompt d'extraction d'origine, activer
le graphe faisait tomber le rappel global de 79 % à 64-71 %, en évinçant des réponses que
le dense trouvait, et abaisser `graph_top_k` le rétablissait. L'explication paraissait
structurelle : la stratégie graphe classe ses candidats par nombre de sauts, confiance et
récence, trois grandeurs qui ne mesurent aucune pertinence par rapport à la question.

Elle était fausse. Le même corpus, la même fusion et le même `graph_top_k` par défaut
donnent 86 % dès que le prompt d'extraction produit des relations entre entités nommées
au lieu de propriétés isolées. Ce qui inondait la fusion n'était pas le nombre de
candidats du graphe, c'était leur inutilité. Un graphe pauvre en relations pertinentes
apporte du bruit avec de bons rangs ; un graphe correct n'a pas besoin qu'on le brime.
La conséquence pratique est que la qualité du prompt d'extraction est le premier levier de
rappel de toute cette couche, très loin devant les réglages de la recherche.

L'extracteur du graphe lit le contenu des messages pour en tirer des entités et des
relations, ce qui expose le service à une forme d'injection de prompt : un message
adversaire qui nomme une entité réelle du workspace peut faire produire par le modèle une
relation fabriquée, affichée avec une confiance élevée, à propos de cette entité. Les revues
de sécurité n'ont pas trouvé de façon pour ce message de sortir de son propre workspace,
de s'attacher à des messages source qu'il n'a pas lui-même fournis (`graph_relation_sources`
ne référence que les messages effectivement passés à l'extracteur pour ce job), ou
d'échapper au schéma JSON contraint et aux bornes que `Validate` impose (types connus,
`confidence` dans `[0, 1]`, workspace cohérent). C'est le pire cas concret qu'une revue ait
établi, pas une preuve qu'aucun autre n'existe : une relation fabriquée mais correctement
bornée et scopée reste un risque pour la qualité des faits restitués, à traiter en aval par
l'appelant du même oeil critique que le reste d'une sortie de modèle de langage.
