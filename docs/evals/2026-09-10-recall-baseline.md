# Évaluation du rappel sémantique : mesure de référence

Cette note consigne la première exécution de `go run ./eval` contre le vrai
Ollama et le vrai modèle d'embedding, comme le prévoit la tâche 17. Elle
mesure les critères d'acceptation 2 et 3 de la spec (paraphrase, termes
rares), qui ne veulent rien dire avec le faux embedder à hash utilisé par les
tests d'intégration, et sert de point de départ à la calibration de
`retrieval.minimum_score`.

## Les trois résultats de ce run

1. **Paraphrase 6/6.** Le critère d'acceptation 2 (une paraphrase retrouve un
   ancien message pertinent) était pure hypothèse jusqu'à ce run : c'est la
   première fois qu'il est mesuré contre le vrai modèle plutôt que contre le
   faux embedder à hash des tests d'intégration.
2. **Lexical 4/4.** Le critère d'acceptation 3 (une recherche exacte retrouve
   les noms et termes rares) est vérifié de la même façon.
3. **Le score fusionné n'est pas exploitable comme plancher de pertinence,
   et ça ne tient pas à la taille de ce corpus.** `retrieval.minimum_score`,
   tel que spécifié, s'applique au score `final` que rend la fusion RRF. Ce
   score n'encode que le rang d'un candidat au sein d'une stratégie
   (`1 / (rrf_k + rang)`), jamais sa distance sémantique réelle : deux
   candidats de même rang reçoivent le même score, qu'ils soient pertinents
   ou non. La preuve est dans les chiffres de ce run même : les six scores
   « pertinents » qui ne doivent rien au lexical (0,0161 et 0,0163) et les
   scores « non pertinents » du même run partagent des valeurs identiques au
   dix-millième près.

   | Score  | Résultats pertinents | Résultats non pertinents |
   |--------|----------------------|---------------------------|
   | 0,0144 | 0                    | 1                         |
   | 0,0147 | 0                    | 3                         |
   | 0,0149 | 0                    | 5                         |
   | 0,0151 | 0                    | 5                         |
   | 0,0153 | 0                    | 9                         |
   | 0,0156 | 0                    | 9                         |
   | 0,0158 | 0                    | 8                         |
   | 0,0161 | 2                    | 6                         |
   | 0,0163 | 4                    | 4                         |
   | 0,0327 | 4                    | 0                         |

   Les deux dernières lignes avant le palier lexical (0,0161 et 0,0163) sont
   partagées par les deux colonnes : le score fusionné place, à la valeur
   près, un vrai résultat de paraphrase et un voisin sans rapport rendu par
   une tout autre requête. Aucun seuil ne peut trancher entre deux valeurs
   identiques. Plus de données ne changerait que l'étalement des rangs, pas
   le fait que le rang, à lui seul, ne porte aucune information de distance :
   ce n'est pas un artefact de ce corpus de smoke test, c'est ce que RRF fait
   par construction. Un plancher de pertinence qui fonctionne devrait porter
   sur le score brut de la stratégie dense avant fusion, où la similarité
   cosinus garde son sens ; ce score existe déjà, séparément, dans
   `Result.Scores["dense"]`, sans rien à ajouter au modèle pour l'exposer.
   Détail complet dans la section de calibration plus bas.

## Environnement de la mesure

- Modèle : `nomic-embed-text-v2-moe` (architecture `nomic-bert-moe`, 475M
  paramètres, 768 dimensions, quantization F16), servi par Ollama 0.33.0 en
  local.
- Base : Postgres 16 + pgvector, lancée via `make up`, vidée par la commande
  elle-même après le run (voir plus bas).
- Configuration : `config.yaml` du dépôt, avec `graph.enabled` forcé à
  `false` par la commande elle-même (indépendamment de ce que porte le
  fichier) et `retrieval.final_top_k` fixé à la valeur de `-k` (5 par
  défaut). `retrieval.minimum_score` n'est **pas** modifié par la commande :
  il reste tel que chargé depuis `config.yaml`, c'est-à-dire `null` au moment
  de cette mesure. C'est volontaire : la mesure sert justement à observer le
  signal brut avant tout filtre, pour ensuite décider s'il faut en poser un.
- Jeu de données : `eval/dataset.json`, 8 conversations et 12 requêtes
  annotées en français (jardin, cuisine, outillage, bricolage, voyage,
  animaux, musique, plus une conversation privée pour le cas ACL), réparties
  en 6 paraphrases, 4 termes rares/lexicaux, 1 requête hors sujet et 1
  requête ACL.
- Commande : `make eval` (équivalent à
  `POSTGRES_DSN=... go run ./eval -config config.yaml`).

## Correction apportée après une première version de cette note

Une première version de ce document reposait sur une commande dont la
classification des scores avait un bug : chaque résultat d'une requête
héritait de l'étiquette « pertinent » ou « non pertinent » de la requête
entière, pas de sa propre pertinence. Comme une recherche réussie rend
typiquement un vrai résultat entouré de plusieurs voisins sans rapport (voir
plus bas), la quasi-totalité de ce que la commande imprimait comme « scores
des résultats pertinents » était en réalité du bruit de remplissage
étiqueté par erreur. La revue de cette tâche l'a repéré arithmétiquement à
partir de la sortie imprimée elle-même (50 valeurs côté « pertinents », soit
dix requêtes réussies fois cinq résultats chacune ; 10 valeurs côté « non
pertinents », soit deux requêtes échouées fois cinq). Le bug a été corrigé
dans `eval/main.go` : chaque résultat est maintenant classé selon que son
propre `SourceMessageIDs` recoupe ou non les `expected_ids` de sa requête,
plus jamais selon le succès global de la requête. Ce document a été
réécrit à partir d'un nouveau run avec cette correction ; les chiffres de
rappel (paraphrase 6/6, lexical 4/4, negative 0/1, acl 0/1) sont inchangés,
seules les deux distributions de score et l'analyse qui en dépend le sont.

## Sortie complète du run (avec la classification corrigée)

```
workspace : eval_0a2204e3

ingestion : 8 conversations en 761ms

REQUÊTE                    FAMILLE      OK     TROUVÉ
paraphrase_maturite        paraphrase   oui    [aime_tomates allergie tomates_vertes cultivar motoculteur chopin voyage_cevennes]
paraphrase_gouts           paraphrase   oui    [aime_tomates allergie perceuse chopin voyage_cevennes tomates_vertes cultivar]
paraphrase_restriction     paraphrase   oui    [aime_tomates allergie perceuse chopin tomates_vertes cultivar voyage_cevennes]
paraphrase_perceuse        paraphrase   oui    [motoculteur perceuse tomates_vertes cultivar chopin voyage_cevennes]
paraphrase_randonnee       paraphrase   oui    [voyage_cevennes tomates_vertes cultivar motoculteur perceuse chopin]
paraphrase_chat            paraphrase   oui    [chat_nocturne perceuse tomates_vertes cultivar motoculteur voyage_cevennes]
terme_rare_cultivar        lexical      oui    [tomates_vertes cultivar chopin perceuse aime_tomates allergie motoculteur]
terme_rare_machine         lexical      oui    [motoculteur perceuse tomates_vertes cultivar chopin aime_tomates allergie]
terme_rare_perceuse        lexical      oui    [perceuse motoculteur aime_tomates allergie chopin tomates_vertes cultivar]
terme_rare_chopin          lexical      oui    [chopin motoculteur perceuse voyage_cevennes aime_tomates allergie]
hors_sujet                 negative     non    [voyage_cevennes perceuse tomates_vertes cultivar chopin motoculteur]
acl_budget                 acl          non    [motoculteur voyage_cevennes tomates_vertes cultivar perceuse chopin]

acl          0/1
lexical      4/4
negative     0/1
paraphrase   6/6

Recall@5 global : 10/12 (83%)

scores des résultats pertinents :     [0.0161 0.0161 0.0163 0.0163 0.0163 0.0163 0.0327 0.0327 0.0327 0.0327]
scores des résultats non pertinents : [0.0144 0.0147 0.0147 0.0147 0.0149 0.0149 0.0149 0.0149 0.0149 0.0151 0.0151 0.0151 0.0151 0.0151 0.0153 0.0153 0.0153 0.0153 0.0153 0.0153 0.0153 0.0153 0.0153 0.0156 0.0156 0.0156 0.0156 0.0156 0.0156 0.0156 0.0156 0.0156 0.0158 0.0158 0.0158 0.0158 0.0158 0.0158 0.0158 0.0158 0.0161 0.0161 0.0161 0.0161 0.0161 0.0161 0.0163 0.0163 0.0163 0.0163]

Le seuil minimum_score se pose entre les deux distributions,
et seulement si elles se séparent nettement.

workspace eval_0a2204e3 supprimé (relancer avec -keep pour le conserver)
```

(Les lignes `slog` d'info par requête, redondantes avec le tableau, ont été
omises ici pour la lisibilité. Dix scores « pertinents » : un par requête qui
a trouvé son résultat cible, soit exactement les 6 paraphrases et les 4 cas
lexicaux. Cinquante scores « non pertinents » : les quatre voisins sans
rapport rendus en plus de ce résultat cible sur chacune de ces dix requêtes
(4 × 10 = 40), plus les cinq résultats de chacune des deux requêtes
`negative` et `acl` (2 × 5 = 10), qui ne peuvent par construction produire
aucun score « pertinent » puisqu'elles n'ont aucun `expected_ids`.)

## Lecture des chiffres

**Paraphrase (6/6, 100%) et lexical (4/4, 100%).** Les six paraphrases
retrouvent chacune leur message cible dans le top 5, sans jamais partager de
mot avec la requête pour au moins trois d'entre elles (« Est-ce que les
tomates de Paul étaient mûres ? » retrouve un message qui dit « encore
vertes », l'inverse sémantique du mot employé dans la question). Les quatre
requêtes à terme rare ou nom propre (« Rose de Berne », « Staub PP2X »,
« Bosch GSR 18V-60 FC », « Fantaisie Impromptu ») retrouvent chacune leur
message et, à chaque fois, le candidat qui porte le terme exact ressort avec
un score composite net au-dessus du reste (0,0328 contre 0,014–0,016 pour le
reste de la liste), preuve que la fusion dense + lexical fonctionne bien
comme prévu : ces quatre requêtes sont les seules du jeu à obtenir une
contribution lexicale non nulle. C'est le résultat qui compte le plus pour
cette tâche : les critères 2 et 3 sont satisfaits contre le vrai modèle.

**Second constat comportemental : tant que `retrieval.minimum_score` reste à
`null`, une recherche rend toujours jusqu'à `final_top_k` résultats, aussi
peu pertinents soient-ils.** La stratégie dense (plus proches voisins) n'a
pas de seuil absolu : elle rend ses `k` voisins les plus proches dès qu'il
existe la moindre unité vectorielle dans le workspace, quelle que soit la
distance sémantique réelle à la question posée. Ce n'est écrit nulle part
ailleurs dans le dépôt avant cette mesure, et sans elle un opérateur qui
verrait le service rendre cinq extraits sur une question hors sujet
pourrait raisonnablement conclure qu'il invente des souvenirs, alors qu'il
rend fidèlement la chose la moins éloignée qu'il ait trouvée. C'est
exactement ce que ce run a rendu visible : voir les familles `negative` et
`acl` plus bas. La spec anticipe ce cas en un mot (le service ne doit pas
forcer cinq résultats quand la pertinence est basse), et le mécanisme qui
l'empêcherait, `minimum_score`, est celui dont le constat en tête de ce
document montre qu'il ne peut pas remplir ce rôle tant qu'il s'applique au
score fusionné, quelle que soit la taille du corpus sur lequel on le
calibrerait (voir la section de calibration plus bas). Ce comportement est
documenté dans le README, dans `config.example.yaml` et ici : un appelant
est censé regarder le score `final` de chaque résultat avant de l'exploiter,
pas seulement le fait qu'un résultat existe.

**Negative (0/1) et ACL (0/1) : ce qui échoue, et ce qui ne prouve rien
sur l'autorisation.** Les deux familles échouent au sens strict imposé par
cette commande (« la recherche ne doit rendre absolument rien »), pour
exactement la raison décrite au paragraphe précédent. Sur un corpus de
smoke test d'une douzaine d'unités, appartenant toutes au même demandeur
(`user:paul`), une requête hors sujet ou une requête sur un contenu qui lui
est inaccessible renvoie donc quand même les quelques souvenirs de
`user:paul` les moins mauvais parmi ce qu'il possède. Vérification faite sur
les colonnes TROUVÉ ci-dessus : le message `budget` (celui de la conversation
privée `eval_prive`, dont `user:paul` n'est pas participant) n'apparaît
**jamais** dans les résultats de `acl_budget`, sur aucun des quatre runs
effectués pendant l'implémentation (trois avant la correction du bug de
classification décrite plus haut, un après) ; ce qui est rendu, ce sont d'autres
souvenirs de `user:paul` lui-même, jamais le contenu protégé. C'est un
artefact de la taille du corpus et de l'absence de seuil, pas un défaut
d'autorisation.

Pour être clair sur ce que la famille `acl` établit ici et ce qu'elle
n'établit pas : ce n'est pas elle qui prouve le critère 4. Ça, ce sont les
tests d'intégration (avec le faux embedder, sur la plomberie SQL et les
jointures de garde) et le harnais de bout en bout nommé par critère
d'acceptation, qui couvrent ce cas de façon reproductible et sans dépendre
d'Ollama. Le rôle de `acl_budget` dans cette évaluation-ci se limite à
attraper une régression catastrophique contre le vrai modèle (un message
protégé qui se mettrait à fuir dans les résultats à cause, par exemple, d'un
bug de construction de requête propre au chemin sémantique) ; ce n'est pas
la mesure qui rendrait crédible ou non le critère 4 en général.

**Ce qui a été vérifié pour écarter l'hypothèse d'un bug.** Conformément à
la consigne de ne pas conclure trop vite sur un mauvais score de la famille
paraphrase, les deux causes les plus fréquentes de
mauvais rappel avec les modèles Nomic ont été vérifiées avant d'accepter les
83% globaux comme un résultat propre :

- **Construction de `embedding_text`** (`internal/memory/units.go`,
  `BuildUnit`) : le texte indexé porte le préambule des participants, un
  bloc de contexte optionnel puis « Message principal : auteur : contenu »,
  sans jamais y injecter le préfixe d'encodage. Confirmé en lisant le code
  avant l'implémentation de cette tâche.
- **Préfixes `search_document:` / `search_query:`** (`internal/llm/client.go`)
  : `Embed` applique toujours `docPrefix`, `EmbedQuery` toujours
  `queryPrefix`, chacun exactement une fois, à l'intérieur du client
  d'embedding, jamais composés en amont par `internal/memory` (qui ne fait
  que construire le texte brut ; voir le commentaire de
  `Searcher.embedQuery` et de `BuildQueryText`). Un double préfixage ou un
  préfixe inversé aurait dégradé silencieusement tout le rappel dense,
  paraphrase compris ; le score de 100% sur la famille paraphrase suggère
  que ce n'est pas le cas ici.

Ces deux points ont été relus dans le code existant avant d'écrire la
commande, précisément parce que la famille paraphrase est celle qui aurait
le plus souffert d'une erreur sur l'un des deux. Le résultat (6/6) rend
l'hypothèse d'un bug de préfixage ou de construction de texte peu probable ;
en tout état de cause, le score de 100% sur les deux familles positives ne
laisse rien à corriger de ce côté-là pour cette mesure.

## Ce que ces chiffres montrent, et ce qu'ils ne montrent pas

Douze requêtes rédigées à la main pour huit conversations inventées, c'est
un test de fumée, pas un banc d'essai. Un Recall@5 de 83% sur un jeu de cette
taille n'a pas la valeur statistique d'un pourcentage mesuré sur un corpus
de production ni sur un jeu de centaines de requêtes réelles : un seul cas
qui bascule change le chiffre de plusieurs points. Ce qui compte ici, ce
n'est pas le pourcentage global, c'est que chacune des deux familles qui
testent une vraie capacité sémantique (paraphrase, terme rare) obtient
100%, et que la vérification manuelle des résultats bruts confirme qu'aucune
fuite de contenu inter-workspace ou inter-ACL n'a eu lieu.

## Calibration de `retrieval.minimum_score`

**Décision : laisser `retrieval.minimum_score` à `null`.**

Avec la classification corrigée, les dix scores « pertinents » sont
[0,0161 0,0161 0,0163 0,0163 0,0163 0,0163 0,0327 0,0327 0,0327 0,0327] et
les cinquante « non pertinents » vont de 0,0144 à 0,0163 (voir le tableau en
tête de ce document). Ce n'est plus la quasi-totale confusion d'avant
correction, mais ça ne se sépare toujours pas, pour deux raisons distinctes,
et la seconde n'est pas une limite de ce corpus : c'est une limite du score
lui-même.

1. **Le palier à 0,0327 sépare parfaitement, mais seulement lui, et il ne
   couvre que la moitié positive de la mesure.** Ces quatre valeurs sont les
   quatre requêtes à terme rare, et ce sont les seules qui obtiennent une
   contribution lexicale en plus de dense ; aucune des cinquante valeurs
   « non pertinentes » ne les approche. Un seuil placé entre 0,02 et 0,03
   isolerait donc ce palier avec une précision et un rappel parfaits, mais
   seulement pour la famille `lexical`. Appliqué à l'ensemble, ce même seuil
   éliminerait aussi les six scores « pertinents » de la famille
   `paraphrase` (0,0161 à 0,0163, tous obtenus par dense seul) : le
   Recall@5 mesuré tomberait de 10/12 à 4/12, en écartant justement les
   requêtes qui mesurent le critère 2. Un seuil qui ferait ça ne calibrerait
   rien, il désactiverait la moitié de ce que cette tâche existe à mesurer.
   Ce point-là resterait vrai sur un corpus de n'importe quelle taille :
   une paraphrase authentique que seul dense retrouve aura toujours ce même
   plafond structurel face à un candidat porté par deux stratégies à la
   fois.

2. **En dessous de 0,02, les deux distributions ne se contentent pas de se
   chevaucher, elles partagent des valeurs identiques, et ce n'est pas
   parce que le corpus est petit : c'est parce que le score fusionné
   n'encode qu'un rang.** Les six scores « pertinents » qui ne doivent rien
   au lexical (0,0161 pour deux d'entre eux, 0,0163 pour les quatre autres)
   tombent sur exactement les mêmes deux valeurs que dix des cinquante
   scores « non pertinents » (six à 0,0161, quatre à 0,0163) : des nombres
   identiques au dix-millième près, pas un chevauchement approximatif. La
   fusion RRF calcule `1 / (rrf_k + rang)` à partir du rang d'un candidat au
   sein d'une stratégie, jamais de sa distance sémantique réelle : deux
   candidats de même rang reçoivent donc le même score que l'un soit un
   excellent résultat et l'autre le moins pire d'un lot sans rapport. Un
   exemple concret tiré de ce run : sur `paraphrase_maturite`, le message
   correct (`tomates_vertes`) ressort deuxième derrière un message hors
   sujet (`aime_tomates`, plus proche au sens du cosinus pour cette requête
   bien qu'il ne parle pas de maturité) ; le score RRF du bon message y vaut
   `1/62` (0,0161), rigoureusement le même score qu'obtiendrait n'importe
   quel autre message classé deuxième par dense sur n'importe quelle autre
   requête, pertinent ou non. Grossir le corpus étalerait les rangs
   possibles (les cinquante scores « non pertinents » de ce run ne prennent
   déjà que neuf valeurs distinctes, celles de `1 / (60 + rang)` pour les
   rangs 1 à 9, parce que `MergeAdjacent` laisse des rangs au-delà de 5
   atteindre quand même le top 5 final en absorbant le score d'un message
   mieux classé de la même conversation), mais grossir le corpus ne
   restitue pas l'information de distance que le rang a déjà effacée : un
   excellent résultat classé premier et un résultat médiocre classé premier
   dans une autre requête resteront toujours au même score, quelle que soit
   la taille du corpus. `retrieval.minimum_score`, tel que spécifié,
   s'applique à ce score fusionné : il ne peut donc jamais servir de
   plancher de pertinence, pas seulement « pas encore, faute de données ».

Poser un `minimum_score` sur le score fusionné irait contre l'esprit de la
section 18 de la spec, qui interdit justement de l'inventer, et ce serait
pire qu'inventer un chiffre : ce serait poser un seuil qui ne peut
structurellement pas faire ce qu'on attend de lui, quelle que soit la
valeur choisie (point 1) ou la taille du corpus mesuré (point 2). Un
plancher de pertinence qui fonctionne doit porter sur une grandeur qui
garde une notion de distance, pas de rang : le score brut de la stratégie
dense avant fusion, déjà porté séparément par `Result.Scores["dense"]`
(voir `RawScores` dans `internal/memory/fusion.go`), rien à ajouter au
modèle pour l'exposer. Ça n'est pas déjà vérifié : sur ce même run, la
similarité cosinus brute ne classe pas non plus systématiquement le bon
message devant tout le reste (l'exemple `paraphrase_maturite` ci-dessus
tient aussi au niveau du score dense seul, `aime_tomates` y devançant
`tomates_vertes`), donc calibrer un seuil sur le score dense demande, lui
aussi, une vraie mesure plutôt qu'une supposition. Mais c'est la bonne
quantité à mesurer, puisque contrairement au score RRF elle varie
continûment avec la pertinence plutôt que de la réduire à un rang entier.
La prochaine étape, si ce filtre s'avère nécessaire, est donc double :
changer ce qui est mesuré (le score dense, pas le score fusionné) et
rejouer cette commande sur un corpus représentatif de la volumétrie de
production (au moins plusieurs centaines d'unités par workspace, avec un
mélange réaliste de sujets).

## Conclusion

Les critères d'acceptation 2 et 3 sont vérifiés contre le vrai modèle
(100% sur les deux familles qui les couvrent). Le critère 4 (pas de fuite
vers un agent non autorisé) est vérifié manuellement sur les résultats bruts
de la requête ACL, malgré un score de famille à 0% dû à une exigence plus
stricte que le critère lui-même (voir plus haut). `retrieval.minimum_score`
reste `null` : tel que spécifié, appliqué au score fusionné par RRF, ce
seuil ne peut structurellement pas servir de plancher de pertinence, sur ce
corpus comme sur n'importe quel autre, puisque ce score n'encode qu'un rang
et jamais une distance. Calibrer un vrai plancher demandera de le poser sur
le score brut de la stratégie dense avant fusion, puis de mesurer celui-là
sur un corpus de taille représentative de la production.
