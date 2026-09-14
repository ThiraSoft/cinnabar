<p align="center">
  <img src="docs/assets/logo.svg" alt="Cinnabar" width="112">
</p>

<h1 align="center">Cinnabar</h1>

<p align="center">
  Mémoire hybride pour agents conversationnels.
</p>

---

Cinnabar enregistre les messages d'une conversation, les indexe et rend les
souvenirs utiles pour répondre à une question donnée. Le service expose une
API HTTP authentifiée par token et cloisonne strictement les workspaces et
les identités entre tenants.

## Points clés

- **Recherche hybride** : vectorielle (pgvector), lexicale et, en option, par
  graphe de connaissances. Les résultats sont fusionnés puis éventuellement
  réordonnés par un modèle.
- **Sait se taire** : quand rien dans le workspace ne répond à la question,
  la recherche peut ne rien rendre plutôt qu'un voisin lointain.
- **Visibilité fine** : scopes par conversation (`participants`, `workspace`,
  `private`, `explicit`), ACL par souvenir, suppression immédiate.
- **Cohérence au choix** : l'écriture peut attendre que le message soit
  indexé avant de répondre.
- **Robuste** : indexation asynchrone par file de jobs avec reprise et
  lettres mortes. Une panne du graphe ou du reranker dégrade la recherche
  sans la bloquer.
- **Simple à opérer** : un binaire Go et PostgreSQL. Les migrations
  tournent au démarrage.

## Démarrage rapide

Prérequis : Go 1.25, Docker, et un serveur d'embedding compatible OpenAI
(Ollama avec `nomic-embed-text-v2-moe` par défaut).

```bash
make up                      # PostgreSQL + pgvector sur le port 5433
cp config.example.yaml config.yaml

export POSTGRES_DSN="postgres://cinnabar:cinnabar@localhost:5433/cinnabar?sslmode=disable"
# Requises au chargement même graphe désactivé, des valeurs bidon suffisent
export LLM_API_URL="http://localhost:8000/v1" LLM_API_KEY="dummy"

make build
./bin/cinnabar keys create orchestrateur ws1 'user:*,agent:*'   # affiche le token une seule fois
./bin/cinnabar -config config.yaml
```

## Exemple

```bash
TOKEN=<token affiché par keys create>

# Enregistrer un message
curl -s -X POST localhost:8080/v1/messages \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: evt-1' \
  -d '{
    "workspace_id": "ws1", "conversation_id": "conv_8453",
    "author_key": "user:paul", "role": "user",
    "content": "Je suis passé près de mes tomates, elles étaient encore vertes.",
    "consistency": "searchable"
  }'

# Retrouver les souvenirs pertinents
curl -s -X POST localhost:8080/v1/memories/search \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "workspace_id": "ws1", "requester_key": "user:paul",
    "query": "Où en était le potager de Paul ?",
    "include_context_block": true
  }' | jq
```

## API

| Méthode | Route | Rôle |
|---|---|---|
| `POST` | `/v1/messages` | Enregistrer un message |
| `PATCH` | `/v1/messages/{id}` | Modifier un message |
| `DELETE` | `/v1/messages/{id}` | Supprimer un message |
| `POST` | `/v1/conversations` | Déclarer une conversation et son scope |
| `DELETE` | `/v1/conversations/{id}` | Supprimer une conversation |
| `POST` | `/v1/memories/search` | Rechercher des souvenirs |
| `GET` `POST` | `/v1/memories/{id}/acl` | Lister ou accorder un accès |
| `DELETE` | `/v1/memories/{id}/acl/{principal}` | Révoquer un accès |
| `GET` | `/health` `/about` `/debug/stats` | Sondes, sans token |

Les routes sans token ne doivent pas être exposées publiquement : voir
[Déploiement](docs/guide.md#déploiement).

## Configuration

Tout passe par `config.yaml`, à partir de `config.example.yaml`. Les secrets
restent dans l'environnement via la syntaxe `${VARIABLE}` et le fichier est
ignoré par git. Les blocs principaux :

| Bloc | Contenu |
|---|---|
| `embedding` | Modèle et endpoint d'embedding |
| `retrieval` | Stratégies, fusion, seuils de non-réponse |
| `graph` / `extraction` | Graphe de connaissances (désactivé par défaut) |
| `rerank` | Réordonnancement par modèle (désactivé par défaut) |
| `jobs` | File d'indexation, reprises, lettres mortes |

## Développement

```bash
make test               # tests unitaires
make test-integration   # avec PostgreSQL (make up avant)
make lint               # go vet + gofmt
make eval               # mesure du rappel sur le corpus d'évaluation
```

Pour lancer dans un conteneur : `cp .env.example .env` puis
`docker compose --profile app up -d --build cinnabar`.

## Documentation

- [Guide](docs/guide.md) : comportement de la recherche, partage et
  suppression, graphe, configuration avancée, déploiement
- [Spécification](docs/design/2026-09-09-service-memoire-hybride-design.md)
- [Évaluations](docs/evals/)
