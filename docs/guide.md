# Guide de Cinnabar

Ce guide détaille le comportement du service. Pour installer et lancer
Cinnabar, voir le [README](../README.md).

## Ce que rend une recherche

Deux dispositifs permettent à une recherche de **ne rien rendre du tout**
quand rien dans le workspace ne répond à la question.

`retrieval.minimum_dense_score`, à 0,45 par défaut, écarte les candidats de
la stratégie dense dont la similarité cosinus tombe en dessous. La valeur
est basse exprès : elle n'écarte que les voisins vraiment lointains.

`retrieval.no_answer_best_below` et `no_answer_margin_below`, **désactivés
par défaut**, écartent les candidats du dense **en bloc** quand son meilleur
candidat est à la fois lointain et noyé dans un voisinage plat. Sur le corpus
d'évaluation français, 0,58 et 0,05 ferment trois des quatre questions sans
réponse sans coûter une seule vraie réponse. Sur LoCoMo, en anglais, les
mêmes seuils écartent 300 questions qui ont une réponse et font perdre 10
points de rappel : une similarité cosinus absolue dépend de la langue et du
corpus, et aucun couple de seuils ne sert les deux.

Pour l'activer, mesurer d'abord sur son propre corpus la forme du voisinage
dense (`service.debug_search: true` rend `dense_best` et `dense_median`) de
questions qui ont une réponse et de questions qui n'en ont pas, puis choisir
des seuils qui séparent les deux. `make eval` imprime cette forme pour le
corpus d'évaluation.

Les deux ne filtrent que le dense : une correspondance lexicale et un fait du
graphe restent des preuves de pertinence par elles-mêmes. C'est délibéré, et
c'est aussi pour ça que la stratégie lexicale ne cherche pas à répondre à
tout : son silence est une information.

Mettre `minimum_dense_score` à `null` restaure le comportement décrit
ci-dessous, où le service répond toujours quelque chose. Les mesures qui
fondent ces défauts sont dans `docs/evals/2026-09-18-bancs-mempalace.md`.

Sans plancher, et c'est aussi le cas de `retrieval.minimum_score` qui reste
`null` parce qu'il porte sur la mauvaise grandeur,
une recherche rend toujours jusqu'à `final_top_k` extraits, même quand rien
dans le workspace n'est vraiment pertinent pour la question posée. La
stratégie dense n'a pas de plancher : elle classe par proximité vectorielle
et rend ses voisins les plus proches, aussi loin soient-ils sémantiquement,
dès qu'il existe la moindre unité indexée. Ce n'est pas le service qui
invente un souvenir : c'est la chose la moins éloignée qu'il a trouvée, et
il le dit avec un score `final` bas plutôt qu'en refusant de répondre.

L'appelant est donc censé regarder le score `final` de chaque résultat avant
de l'injecter dans un prompt, plutôt que de faire confiance au seul fait
qu'un résultat existe. Poser un plancher fiable demande de l'avoir calibré
sur de vraies conversations : c'est le rôle de `go run ./eval` (voir
`docs/evals/`), qui imprime les distributions de score des
résultats pertinents et non pertinents une fois qu'un corpus de taille
représentative existe.

## Restreindre une recherche

`conversation_ids` borne les ancres possibles à quelques conversations, et
`metadata_filter` à des messages dont les metadata répondent à une condition.
Les deux s'ajoutent à la règle d'accès et ne peuvent que retirer des
résultats. Chaque mémoire rendue porte les metadata de son message d'ancrage.

```json
{
  "workspace_id": "ws1", "requester_key": "agent:village",
  "query": "Qu'est-ce que Ricardo m'a promis ?",
  "conversation_ids": ["nine|player:ricardo", "nine|lore"],
  "metadata_filter": {"all": [
    {"key": "belief", "op": "ne", "value": "non"},
    {"any": [
      {"key": "min_affinity", "op": "lte", "value": 40},
      {"key": "lore_id", "op": "in", "value": ["nine-1"]}
    ]}
  ]}
}
```

Opérateurs `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `in`, `exists`, groupes
`all` et `any`. Une clé absente rend toute feuille fausse sauf `ne` et
`exists: false`. `POST /v1/messages/list` accepte les mêmes restrictions et
rend les messages du plus récent au plus ancien, page par page. Le détail est
dans la [spécification](design/2026-09-14-filtres-metadata-design.md).

`GET /v1/conversations?workspace_id=ws1&requester_key=agent:village&prefix=nine|`
rend les conversations lisibles dont l'identifiant commence par le préfixe,
littéral, dans l'ordre des identifiants, avec `limit` (100 par défaut, au plus
500) et `cursor`. Un client qui range ses conversations sous des identifiants
structurés y retrouve celles d'un même propriétaire.

Chaque fait de `graph_facts` détaille ses sources lisibles dans `sources`,
avec pour chacune `message_id`, `conversation_id` et `metadata`. Un fait
restreint par `conversation_ids` ou `metadata_filter` ne rend que les sources
qui y satisfont, au plus trois par fait.

Le filtre choisit les ancres, pas le contexte : avec `expand_before` ou
`expand_after` non nuls, un extrait peut contenir des voisins qui ne le
satisfont pas.

## Partage explicite et suppression de conversation

Un souvenir de scope `private` ou `explicit` n'est visible que par le
workspace de sa conversation source, et par les principaux qu'une ACL
explicite désigne (table `memory_unit_acl`). Ces deux scopes sont
décoratifs tant que rien n'écrit dans cette table : c'est le rôle des routes
suivantes.

```bash
# Partager une unité (son id est rendu par POST /v1/messages dans
# memory_unit_ids) avec un principal qui ne pouvait pas la lire jusque-là.
curl -s -X POST localhost:8080/v1/memories/$UNIT/acl \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"principal_key":"agent:invite"}'

curl -s localhost:8080/v1/memories/$UNIT/acl -H "Authorization: Bearer $TOKEN"

curl -s -X DELETE localhost:8080/v1/memories/$UNIT/acl/agent:invite \
  -H "Authorization: Bearer $TOKEN"
```

La règle retenue est qu'on ne partage que ce qu'on peut lire : l'appelant ne
peut ajouter un principal à l'ACL d'une unité que si son token peut incarner
au moins une identité participante de la conversation source, ou si cette
conversation est déclarée en scope `workspace`. Un scope `private` ou
`explicit` ne s'ouvre jamais par la seule participation : le service ne
consulte `memory_unit_acl` que pour la recherche, jamais pour décider si un
appelant peut lui-même écrire une ACL, donc **aucune** ACL, même détenue par
l'appelant, ne rend une conversation `private`/`explicit` partageable. C'est
délibéré, pas une lacune : voir "Amorcer un scope `explicit`" ci-dessous
pour la façon légitime d'en arriver là. Un appelant qui ne pourrait pas lire
l'unité par la recherche (conversation d'un autre workspace, supprimée, ou
de scope `participants`/`private`/`explicit` sans y participer) se voit
refuser le partage : 404 si la conversation est dans un autre workspace, pour
ne jamais confirmer qu'un identifiant d'unité deviné en désigne une chez
quelqu'un d'autre (les identifiants d'unité sont des uuid v5 déterministes
sur le message ancre, donc devinables à partir d'un id de message qui aurait
fuité) ; 403 dans son propre workspace si l'identité ne correspond à aucun
participant.

Le principal bénéficiaire (`principal_key`) n'est en revanche pas contraint
aux motifs d'identité du token appelant : un token limité à `user:paul` peut
très bien partager avec `agent:tout-autre`. Ce n'est pas un accès en blanc
pour autant, borné qu'il est par le fait qu'exercer ce partage exige, plus
tard, un token à part réellement autorisé pour ce workspace et cette
identité précise.

Lister ou révoquer une ACL (`GET`/`DELETE .../acl`) suit une règle plus
permissive que l'octroyer : y participer suffit, quel que soit le scope,
`private`/`explicit` compris. Octroyer élargit l'accès et doit donc rester
strict ; révoquer ne fait que le retirer, et lister ne dit à un participant
que qui peut déjà lire sa propre conversation. Cette asymétrie évite qu'un
`POST /v1/conversations` qui fige une conversation en `private` après coup
ne rende un octroi existant irrévocable autrement que par SQL direct.

### Amorcer un scope `explicit`

Puisqu'une ACL ne peut jamais être posée sur une conversation déjà
`private`/`explicit`, la façon d'en arriver à un scope `explicit`
correctement peuplé est dans l'ordre : déclarer la conversation en
`participants` (le défaut), y partager les unités voulues pendant qu'elle
l'est encore, puis la re-déclarer (`POST /v1/conversations`) en `explicit`
une fois les partages en place. Les ACL déjà posées restent valides après
le changement de scope ; seule la possibilité d'en ajouter de nouvelles par
la seule participation disparaît.

Re-déclarer une conversation qui existe déjà demande la même autorisation que
la supprimer : incarner l'un de ses participants, ou bénéficier d'un scope
`workspace`. Deux conséquences à connaître avant de s'y heurter. Une
conversation déclarée en `participants` avec une liste de participants vide
n'a personne pour la re-déclarer : il faut y poster un message d'abord, ce qui
inscrit son auteur comme participant. Et une conversation supprimée n'est plus
re-déclarable du tout ; elle rend `404`, comme une conversation d'autrui.
Ressusciter une conversation supprimée n'est pas prévu : les messages y
seraient acceptés mais aucun ne pourrait jamais ressortir de la recherche. La séquence ci-dessus passe donc sans rien de plus, celui qui
déclare étant participant de sa propre conversation. Un `conversation_id`
qui existe dans un autre workspace rend `404`, comme partout ailleurs ; dans
le bon workspace mais hors de portée de l'appelant, `403`. Sans ces deux
contrôles, `POST /v1/conversations` suffisait à basculer la conversation d'un
voisin en `workspace` puis à la lire entièrement.

```bash
curl -s -X DELETE localhost:8080/v1/conversations/$CONVERSATION_ID \
  -H "Authorization: Bearer $TOKEN"
```

`DELETE /v1/conversations/{id}` masque la conversation (`deleted_at`) et
désactive toutes ses unités dans la même transaction : elle disparaît
immédiatement de toute recherche, dense comme lexicale, même via une ACL
explicite. Les messages restent en base comme source de vérité et pour
l'audit. Suit la même règle d'accès que le partage (participation ou scope
`workspace`), pas la règle permissive de la gestion des ACL : supprimer une
conversation entière est nettement plus destructeur que révoquer une seule
ACL, donc un token limité à une identité qui ne participe pas à la
conversation (ou qui n'y participe que dans un autre workspace) ne peut pas
la purger, même s'il pourrait par ailleurs lister ou révoquer ses ACL. Une
conversation d'un autre workspace, ou déjà supprimée, rend 404 plutôt que de
confirmer son existence ; une conversation dans le bon workspace mais dont
l'appelant n'est ni participant ni bénéficiaire du scope `workspace` rend
403. L'appel est rejouable sans effet une fois la suppression faite.

## Configuration

`config.example.yaml` sert de base à `config.yaml`. Les valeurs sensibles
(`POSTGRES_DSN`, `LLM_API_URL`, `LLM_API_KEY`) s'expriment en
`${VARIABLE}` et sont résolues depuis l'environnement au chargement.

`graph.enabled` reste à `false` par défaut, y compris en l'absence du bloc
`graph:` dans un `config.yaml` : voir la section « Graphe de connaissances »
ci-dessous pour ce que l'activer change et implique.

`config.yaml` contient des secrets une fois les `${VARIABLE}` résolus (ou
pire, si quelqu'un les y écrit en dur au lieu de les laisser dans
l'environnement). Le fichier est ignoré par git (`.gitignore`) précisément
pour ça : les secrets vivent dans l'environnement ou dans un `.env` non
suivi, jamais commités dans une copie de la configuration.

Le délai laissé aux requêtes HTTP en vol pour finir à l'arrêt
(`service.shutdown_grace`) et celui laissé à un job pour se terminer
(`config.JobHandlerTimeout`, 5 minutes, plus `jobs.reclaim_after` avant
qu'un job resté "running" ne soit repris) ne sont pas la même horloge. Un
`terminationGracePeriodSeconds` Kubernetes calé sur `shutdown_grace` (30s)
peut donc envoyer un `SIGKILL` en plein traitement d'un job d'embedding : ce
n'est pas un bug, `jobs.reclaim_after` reprendra ce job plus tard, mais
c'est un compromis à connaître avant de régler ce délai, pas une surprise
à découvrir en production.

## Réordonnancement des extraits

`rerank.enabled: true` fait relire les extraits par un modèle, qui les
reclasse en lisant la question et les extraits ensemble. Désactivé par
défaut, parce que c'est le seul appel de modèle du chemin de recherche et que
le service est conçu pour ne pas en dépendre.

Ce que la mesure donne, sur le corpus d'évaluation : le Recall@5 passe de
73 % à 78 %, pour environ 220 ms par recherche. Il récupère les deux seules
requêtes dont la réponse était entre le rang six et le rang dix, et atteint
donc exactement le plafond disponible. Aucun signal déjà présent ne les
remonte : l'embedder préfère réellement les distracteurs, et l'écart de score
est trop grand pour un biais de récence.

Une panne du modèle fait retomber sur l'ordre de la fusion et n'empêche
jamais la recherche de répondre. Le score `final` porte alors l'ordre du
réordonnanceur, et le score de fusion reste rendu sous la clé `fusion`.

## Graphe de connaissances

`graph.enabled: true` ajoute une troisième stratégie de recherche, à côté du
dense et du lexical : une traversée de relations entre entités (« Paul »,
« l'équipe compta », « le potager de Paul »...), extraites des messages par
un modèle de langage. Ce que ça change concrètement, avant de basculer le
drapeau :

- **un appel au modèle d'extraction par message ingéré**, plus un appel à
  l'embedder pour vectoriser les faits extraits, en plus de l'appel à
  l'embedder qui existe déjà pour le message lui-même. Le vecteur du fait est
  ce qui permet à la recherche de préférer les faits qui répondent à la
  question: sans lui, elle rend le voisinage de l'entité citée dans un ordre
  qui ignore la question. Un extracteur lent ou coûteux se
  paie donc à chaque message, pas seulement à la recherche ;
- **deux types de jobs de plus** dans la file (`graph_extract`, posé à
  chaque message, et `graph_reeval`, posé quand un message source est édité
  ou supprimé), qui viennent s'ajouter à `embed`. Aucun des deux n'est
  consommé tant que `graph.enabled` reste à `false` : voir la note plus bas
  sur le sort d'un job resté en file après une désactivation ;
- **un gain de rappel qui dépend entièrement du prompt d'extraction.** Sur
  le corpus d'évaluation, le graphe activé porte le Recall@5 global de 79 %
  à 86 %, en répondant à une question qu'aucune des deux autres stratégies
  ne sait traiter. Mais avec la version précédente du prompt, le même
  graphe faisait *tomber* le rappel à 64-71 %, en inondant la fusion de
  relations sans rapport avec la question. Le prompt est donc la pièce à
  surveiller si vous le modifiez, et
  `docs/evals/2026-09-10-recall-graphe.md` explique comment le
  mesurer en une minute par itération.

L'extracteur est un endpoint de complétion compatible OpenAI (vLLM, Ollama,
OpenAI...), configuré par `extraction.*` et deux variables
d'environnement :

- `LLM_API_URL` : l'URL de base de l'endpoint, référencée par
  `extraction.base_url` et `rerank.base_url` ;
- `LLM_API_KEY` : la clé portée par `extraction.api_key` et
  `rerank.api_key`.

Si le fournisseur exige des en-têtes HTTP en plus, `extraction.headers` et
`rerank.headers` les ajoutent à chaque appel.

Ces deux variables doivent être définies dès que `config.example.yaml` les
référence, même quand `graph.enabled` vaut `false` : le chargement de la
configuration échoue sur une variable d'environnement absente, qu'elle
serve ou non. Des valeurs bidon suffisent tant que le graphe reste
désactivé (voir le démarrage rapide du README).

Une fois activé, le graphe **ne se remplit qu'à partir des messages ingérés
après l'activation**, jamais rétroactivement : un message déjà en base au
moment où `graph.enabled` passe à `true` n'a jamais posé de job
`graph_extract` et n'en posera pas tout seul. Rattraper un historique déjà
présent se ferait en reposant des jobs `graph_extract` à la main, message
par message ou par lot ; ce n'est pas outillé ici, faute d'un contrôle de
débit vers le modèle d'extraction que rien dans le service ne fournit encore.

Une panne de l'extracteur **dégrade la recherche sans rendre le service
indisponible** : un job `graph_extract` qui échoue repart en backoff comme
n'importe quel job, le message reste parfaitement consultable par le dense
et le lexical entre-temps, et une recherche dont la stratégie graphe échoue
répond quand même avec ce que le dense et le lexical ont trouvé (le
diagnostic de la panne reste visible dans les logs et, si `debug_search` est
activé, dans `Debug.StrategyErrors`).

Désactiver le graphe après l'avoir activé (`graph.enabled` remis à `false`)
laisse d'éventuels jobs `graph_extract`/`graph_reeval` non encore traités en
file : ils tombent alors dans le circuit générique du runner, faute de
handler enregistré pour leur type, et finissent proprement en lettre morte
après `jobs.retry_limit` tentatives, sans bloquer les jobs `embed`.

## Dans un conteneur

`cp .env.example .env` puis `docker compose --profile app up -d --build
cinnabar` construit l'image et démarre le service (le profil `app` évite que
`make up`, utilisé avant les tests d'intégration, ne tente de le démarrer
sans `config.yaml`). Sans `.env`, Compose substitue une chaîne vide aux
variables `LLM_API_URL`/`LLM_API_KEY` qu'il transmet au conteneur,
une base d'URL vide étant la source d'erreur la plus confuse à
diagnostiquer le jour où `graph.enabled` passe à `true`. Sur macOS,
l'embedder doit viser `http://host.docker.internal:11434/v1` plutôt que
`localhost` pour joindre un Ollama qui tourne sur l'hôte depuis l'intérieur
du conteneur.

## Déploiement

`/health`, `/about` et `/debug/stats` ne demandent pas de token, par
conception : une sonde Kubernetes n'en a pas à présenter, et `/debug/stats`
n'expose que des agrégats, sans contenu de message ni identité. Ça n'en fait
pas pour autant une route publique. Elle décrit la forme du service (nombre
de conversations, de messages et d'unités, profondeur de file, lettres
mortes, état du pool de connexions), ce qui est exactement ce qu'un attaquant
regarde en premier et exactement ce qu'il suffit de marteler pour peser sur
la base.

Le port d'écoute ne doit donc pas être routable depuis l'extérieur. En
pratique : exposer le service derrière un ingress qui ne publie que `/v1/` et
`/health`, ou réserver le port à un réseau interne. Si un jour ces routes
doivent vivre sur un port séparé, c'est ce paragraphe qu'il faudra corriger
en premier.
