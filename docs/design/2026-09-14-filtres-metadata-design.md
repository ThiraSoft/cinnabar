# Filtres par conversation et par metadata, listage

## Pourquoi

Un client qui tient lui-même la frontière d'isolation (un serveur de jeu, un
orchestrateur de confiance) veut restreindre une recherche à quelques
conversations et à des messages dont les metadata répondent à une condition,
puis réordonner les résultats avec ses propres critères. Aujourd'hui les
metadata sont stockées mais ne ressortent nulle part, et aucune recherche ne
peut se borner à un sous-ensemble de conversations.

Le service reste généraliste : aucune clé de metadata n'a de sens pour lui, il
ne fait que les comparer.

## Ce qui change

### Recherche

`POST /v1/memories/search` accepte deux champs facultatifs :

- `conversation_ids` : liste de conversations. Seules les unités et messages
  de ces conversations peuvent servir d'ancre. Sans borne propre, la taille
  de la requête limite la liste.
- `metadata_filter` : une condition sur les metadata du message d'ancrage.

Les deux se cumulent avec la règle d'accès existante, jamais à sa place : un
filtre ne peut que retirer des résultats. Ils s'appliquent aux trois
stratégies, dans le SQL, avant le `LIMIT`. Pour le graphe, ils portent sur les
messages sources des faits.

Chaque mémoire rendue porte désormais `metadata`, celles du message d'ancrage.
Les scores bruts (`dense`, `lexical`, `graph`) étaient déjà rendus.

L'expansion de contexte autour d'une ancre reste inchangée : les messages
voisins d'une même conversation peuvent ne pas satisfaire le filtre. Le
filtre choisit les ancres, pas le contexte. Un client qui ne veut que les
ancres règle `expand_before` et `expand_after` à zéro.

### Grammaire du filtre

Un nœud est soit une feuille, soit un groupe :

```json
{"key": "belief", "op": "ne", "value": "non"}
{"all": [ ... ]}
{"any": [ ... ]}
```

Opérateurs : `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `in`, `exists`.

- `eq`, `ne` et `in` comparent des valeurs JSON (`in` attend un tableau).
- `lt`, `lte`, `gt`, `gte` comparent deux nombres ou deux chaînes, et sont
  faux si les types diffèrent.
- `exists` attend un booléen.
- Une clé absente rend toute feuille fausse, sauf `ne` (vrai) et
  `exists: false` (vrai).
- La clé désigne un champ de premier niveau de l'objet metadata.

Bornes vérifiées avant d'atteindre la base : profondeur 4, 32 feuilles, 256
valeurs par `in`, clé de 1 à 128 octets. Une grammaire invalide rend 400.

Exemple, le seuil d'affinité avec une exception pour ce qui a été débloqué :

```json
{"any": [
  {"key": "min_affinity", "op": "lte", "value": 40},
  {"key": "lore_id", "op": "in", "value": ["nine-1", "nine-4"]}
]}
```

Le filtre est évalué par une fonction SQL `cinnabar_metadata_match(jsonb,
jsonb)` posée par la migration 006, ce qui garde les requêtes des stratégies
constantes. La validation Go fait autorité ; la fonction suppose un filtre
déjà validé.

### Filtre et index HNSW

Un `WHERE` supplémentaire sur une recherche HNSW peut rendre moins de lignes
que la limite demandée, l'index ne parcourant que `ef_search` voisins avant
filtrage. Quand un filtre est présent, la recherche dense tourne dans une
transaction avec `SET LOCAL hnsw.iterative_scan = relaxed_order`
(pgvector 0.8 ou plus), qui relance le parcours jusqu'à remplir la limite.
L'ordre rendu reste retrié par distance dans la requête.

### Listage

`POST /v1/messages/list` rend les messages lisibles par le demandeur, du plus
récent au plus ancien, sans score :

```json
{
  "workspace_id": "ws1", "requester_key": "agent:village",
  "conversation_ids": ["nine|public"],
  "metadata_filter": {"key": "belief", "op": "ne", "value": "non"},
  "limit": 100, "cursor": "..."
}
```

Réponse : `messages` (id, conversation, séquence, auteur, rôle, contenu,
date, metadata) et `next_cursor` quand il en reste. Même règle d'accès que la
recherche : conversation lisible par scope, ou unité ancrée couverte par une
ACL du demandeur. `limit` vaut 100 par défaut et au plus 500. Le curseur est
opaque pour le client.

## Hors périmètre

- Classement par récence ou importance : le client réordonne lui-même à partir
  des scores bruts et des metadata.
- Index sur les metadata : les volumes visés tiennent sans, et un index GIN
  n'aiderait pas les comparaisons d'ordre.

## Tests

- Validation du filtre en Go, table de cas valides et invalides.
- `cinnabar_metadata_match` en intégration, chaque opérateur et les clés
  absentes.
- Chaque stratégie filtrée en intégration, conversation et metadata, y compris
  qu'un filtre ne rouvre jamais une conversation illisible.
- Handlers : 400 sur filtre invalide et liste trop longue, metadata rendues.
- Listage : ordre, curseur, règle d'accès.
