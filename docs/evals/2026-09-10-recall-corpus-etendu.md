# Rappel mesuré sur un corpus de 272 messages

Cette note remplace les chiffres de `2026-09-10-recall-graphe.md`, qui portaient sur un
corpus de 20 messages. Ce corpus-là était plus petit qu'une page de résultats : cinq
extraits élargis de deux messages de chaque côté peuvent en rendre vingt-cinq, donc la
réponse pouvait contenir toute la base. Ses 100 % ne mesuraient pas grand-chose.

Le nouveau corpus fait **42 conversations, 272 messages, 46 requêtes annotées** en six
familles. Il a été écrit par quatre agents travaillant depuis une même bible de monde, avec
consigne explicite de créer des voisinages sémantiques : plusieurs conversations parlent de
tomates, plusieurs du déménagement de bureaux, et le potager de Damien à Nîmes existe à
côté de celui de Paul à Villeurbanne pour qu'un service qui les confond échoue.

## L'instrument avant les chiffres

Deux vérifications ont été ajoutées, parce qu'un corpus généré est un instrument dont il
faut douter.

**`go test ./eval/`** valide la structure : identifiants uniques, `expected_ids` qui
pointent des messages réels, rôles cohérents avec les identités, auteurs déclarés
participants, dates croissantes dans une conversation, et surtout qu'aucune requête d'ACL
n'a pour demandeur un participant de la conversation privée qu'elle vise. Une requête
d'ACL fausse est pire qu'absente : elle donne l'illusion que la frontière est mesurée.

**`go run ./eval -audit`** mesure le défaut systématique des corpus générés : une question
de la famille `paraphrase` qui recopie un mot distinctif de son message cible mesure le
lexical et non le sémantique. L'audit rejoue chaque question avec `strategies: ["lexical"]`
et signale celles que le lexical seul résout. Résultat : **0 sur 17**. J'avais d'abord
écrit une heuristique de recouvrement de mots, qui donnait trois faux positifs sur le
corpus d'origine ; la mesurer avec le service lui-même est à la fois plus simple et juste.

## Les chiffres

| Famille | graphe désactivé | graphe activé | ce que la famille mesure |
|---|---|---|---|
| `paraphrase` | **16/17** | 14 à 15/17 | la recherche dense, sans recouvrement lexical |
| `lexical` | **10/10** | 10/10 | le plein texte sur un terme rare |
| `acl` | **3/3** | **3/3** | qu'aucun contenu protégé ne ressorte |
| `temporal` | 3/4 | 1 à 2/4 | l'état le plus récent d'un fait qui change |
| `graph` | 1/8 | **4/8** | un fait atteignable en reliant deux messages |
| `negative` | **3/4** | 0 à 1/4 | ne rien rendre quand rien ne répond |
| **global** | **36/46 (78 %)** | 32 à 35/46 | |

Le chiffre graphe désactivé est **déterministe**: deux exécutions donnent exactement le
même, le pipeline ne contenant aucun modèle génératif. Le chiffre graphe activé varie de
trois requêtes d'une exécution à l'autre, pour la raison exposée plus bas.

Coût d'extraction mesuré : **6 min 17 s pour 272 messages**, soit 1,4 s par message, un
appel au modèle chacun.

## Ce que ces chiffres disent

**La moitié textuelle est bonne.** 16/17 en paraphrase et 10/10 en lexical sur 272
messages avec des voisins volontaires, et l'audit garantit que les questions de paraphrase
ne sont pas résolubles lexicalement. C'est le résultat le plus solide de toute la campagne.

**L'ACL tient, et c'est maintenant mesuré sur la bonne propriété.** Le harnais évaluait une
requête d'ACL sur « elle ne rend rien », ce qui confond deux choses. Une question dont la
réponse est protégée a souvent des voisins parfaitement lisibles, et les rendre n'est pas
une fuite. Le critère est désormais « aucun message protégé ne ressort », vérifié message
par message contre l'ensemble des identifiants que le demandeur n'a pas le droit de lire.
Sur les deux conversations `private` du corpus, contenant un budget chiffré et un nom de
prestataire, **aucune fuite** dans aucune des trois requêtes, et le harnais imprimerait la
fuite nommément si elle avait lieu.

**Le graphe achète une capacité, pas de la moyenne.** Il fait passer sa propre famille de
1/8 à 4/8, un gain de trois requêtes qui dépasse le bruit de l'instrument mesuré plus bas,
et le score global ne bouge pas. Les pertes apparentes d'une paraphrase, d'une temporelle
et d'une négative sont, elles, dans le bruit : il ne faut pas les prendre pour un coût
établi. C'est un arbitrage réel, à trancher selon l'usage : si les questions posées au
service sont du type « où travaille Paul maintenant », le graphe est la seule stratégie qui
répond ; s'il s'agit de retrouver un souvenir par ressemblance, il coûte plus qu'il
n'apporte. Les 4 échecs restants de sa propre famille sont de l'extraction manquante, pas
de la traversée : c'est le levier identifié dans la note précédente.

## Le bruit de l'instrument, avant toute interprétation

L'extraction du graphe passe par un modèle de langage, donc deux exécutions du même
corpus avec la même configuration ne produisent pas le même graphe. Avant de croire à un
écart entre deux mesures, il faut savoir combien l'instrument bouge tout seul.

Un balayage de `graph_top_k`, à corpus et configuration identiques par ailleurs :

| `graph_top_k` | graphe | paraphrase | temporal | négative | global |
|---|---|---|---|---|---|
| 5 | 5/8 | 13/17 | 2/4 | 1/4 | 34/46 (74 %) |
| 10 | 4/8 | 13/17 | 2/4 | 1/4 | 33/46 (72 %) |
| 20 | 4/8 | 15/17 | 2/4 | 0/4 | 34/46 (74 %) |

Le global ne bouge pas de plus de deux points, et les familles bougent dans des directions
incohérentes : la paraphrase est meilleure à 20 qu'à 5, le graphe est meilleur à 5 qu'à 10
et à 20. **`graph_top_k` n'a pas d'effet mesurable sur ce corpus** : ce qu'on voit est la
variance de l'extraction, pas celle du réglage.

Le bruit a ensuite été mesuré directement, par trois exécutions strictement identiques :

| | global | paraphrase | temporal | négative | graphe | lexical | acl |
|---|---|---|---|---|---|---|---|
| tirage 1 | 32/46 | 14/17 | 1/4 | 0/4 | 4/8 | 10/10 | 3/3 |
| tirage 2 | 35/46 | 15/17 | 2/4 | 1/4 | 4/8 | 10/10 | 3/3 |
| tirage 3 | 34/46 | 15/17 | 2/4 | 0/4 | 4/8 | 10/10 | 3/3 |

Trois requêtes d'écart, soit six points, et la variance vient entièrement des familles que
le graphe touche. `graph`, `lexical` et `acl` ne bougent pas d'un tirage à l'autre.

**Graphe désactivé, le pipeline ne contient aucun modèle génératif et le résultat est
exact** : deux exécutions donnent 36/46 au message près. C'est ce chiffre qu'il faut
utiliser pour comparer deux configurations.

Ça condamne aussi une conclusion de la note précédente, où abaisser `graph_top_k` semblait
récupérer les requêtes de paraphrase perdues. À l'échelle de 272 messages, cet effet ne se
reproduit pas.

**Conséquence pratique : un écart de moins de trois requêtes sur quarante-six, soit environ
cinq points, ne veut rien dire.** Toute comparaison de configurations sur ce corpus doit se
faire sur plusieurs tirages, ou porter sur des écarts plus grands que ça. Les seuls écarts
de cette note qui dépassent le bruit sont ceux du plancher au-delà de 0,58, et le gain du
graphe sur sa propre famille.

## Ce que ces chiffres corrigent

**Le plancher de pertinence dense est au bord d'une falaise, pas au centre d'un plateau.**
La note précédente concluait, sur 20 messages, à un plateau `[0,50 ; 0,55]`. Le corpus
étendu montre autre chose :

| `minimum_dense_score` | lexical | negative | paraphrase | temporal | global |
|---|---|---|---|---|---|
| `null` | 10/10 | 0/4 | 16/17 | 3/4 | 33/46 |
| 0,45 | 10/10 | 0/4 | 16/17 | 3/4 | 33/46 |
| **0,52** | **10/10** | 1/4 | **16/17** | **3/4** | **34/46** |
| 0,58 | 8/10 | **4/4** | 14/17 | 2/4 | 32/46 |
| 0,62 | 7/10 | 4/4 | 7/17 | 1/4 | 23/46 |
| 0,70 | 5/10 | 4/4 | 0/17 | 0/4 | 12/46 |

Il n'y a pas de plateau : à partir de 0,58 le plancher échange du rappel réel contre des
négatives, et l'échange devient très mauvais très vite. 0,52 reste le meilleur point parce
qu'il gagne une négative sans rien coûter, mais c'est le dernier avant la chute, et non un
milieu confortable. Le plateau observé sur 20 messages était un artefact de taille : dans
une base minuscule, une question sans réponse n'a rien qui dépasse le seuil ; dans une base
réaliste, il y a toujours quelque chose.

À noter que le plancher touche aussi la famille `lexical`, ce que je n'avais pas anticipé :
retirer un candidat dense change le classement fusionné, et une correspondance lexicale que
le dense renforçait sur le même message d'ancrage perd son appui.

**La famille `negative` demandait un autre instrument que le plancher absolu.** Ce qu'il
faut détecter est qu'une question n'a pas de réponse *dans ce workspace*, ce qui est une
décision relative et pas un seuil sur un score. La section « détection de question sans
réponse » plus bas raconte comment elle a été mesurée puis implémentée : trois des quatre
questions sont désormais fermées, sans perdre une seule vraie réponse.

## La détection de question sans réponse

Le seul défaut produit visible du service était qu'il répond toujours quelque chose. Un
plancher absolu ne pouvait pas le corriger, la section précédente le montre. La forme de la
distribution dense, mesurée par requête, explique pourquoi et donne la solution.

```
requête                          famille      best     médiane  marge
paraphrase_maturite              paraphrase   0.6268   0.5998   0.0270   <- réussit
hors_sujet                       negative     0.4911   0.4604   0.0308
c_paraphrase_style_piano         paraphrase   0.6405   0.6082   0.0323   <- réussit
b_negative_pizza_recipe          negative     0.5571   0.5153   0.0419
c_negative_instrument_damien     negative     0.5394   0.4922   0.0472
d_negative_prix_dresseur         negative     0.5393   0.4751   0.0642
```

Ni la similarité absolue ni la marge ne séparent seules. La requête de paraphrase qui
réussit avec la plus faible marge de tout le corpus, 0,027, est plus resserrée que les
quatre questions sans réponse. Mais son meilleur candidat est à 0,627 quand les leurs
plafonnent à 0,557.

**Une question qui a une réponse présente donc au moins l'un des deux signes** : quelque
chose de vraiment proche, ou quelque chose qui se détache du lot. La règle n'écarte les
candidats du dense que lorsque les deux manquent, et cette conjonction est ce qui la rend
sûre. Les autres stratégies ne sont pas touchées, parce qu'elles savent déjà dire qu'elles
n'ont rien trouvé.

| Réglage | lexical | négative | paraphrase | temporal | global |
|---|---|---|---|---|---|
| détection désactivée | 10/10 | 1/4 | 16/17 | 3/4 | 34/46 (74 %) |
| **best < 0,58 et marge < 0,05** | **10/10** | **3/4** | **16/17** | **3/4** | **36/46 (78 %)** |
| best < 0,58 et marge < 0,07 | 9/10 | 4/4 | 16/17 | 3/4 | 36/46 (78 %) |
| best < 0,60 et marge < 0,08 | 9/10 | 4/4 | 12/17 | 3/4 | 32/46 (70 %) |

Le réglage livré est le deuxième : il ferme trois questions sur quatre **sans coûter une
seule vraie réponse**. Le troisième ferme la quatrième mais perd une requête à terme rare,
et ce troc est refusé délibérément : une question sans réponse qui passe rend du bruit avec
un score honnêtement bas, une vraie réponse perdue est une absence silencieuse.

La quatrième question sans réponse qui subsiste, `d_negative_prix_dresseur`, a une marge de
0,064 : son voisinage n'est pas plat. C'est la limite du dispositif, et elle est assumée.

## Une optimisation essayée, mesurée, et abandonnée

`websearch_to_tsquery` assemble les termes par ET, donc la stratégie lexicale ne se
déclenche que si le message contient tous les mots de la question, mots outils compris.
Sur ce corpus, **quatre requêtes sur quarante-six** seulement trouvent un candidat lexical.
Ça ressemble à un défaut évident.

J'ai donc assemblé les termes par OU, avec `ts_rank_cd` pour trier et un plancher de rang,
et mesuré : **le rappel global tombe de 78 % à 61 %.** La paraphrase perd quatre requêtes,
la détection de non-réponse cesse de fonctionner. La stratégie rend alors des candidats
faibles pour à peu près toutes les questions, qui inondent la fusion exactement comme le
faisait un graphe mal extrait. Le plancher de rang n'y change rien, de 0 à 0,10.

Le changement est annulé, et la leçon est écrite dans le commentaire de la requête pour
que le prochain lecteur ne refasse pas le trajet : **le lexical vaut par sa précision, pas
par son rappel.** Il ne parle que quand il a une vraie raison, et son silence est une
information que la détection de non-réponse exploite. Une stratégie qui répond toujours
quelque chose est une stratégie qui ne dit rien.

## Le réordonnancement, et sa marge exacte

La question posée était : peut-on gagner du rappel en réordonnant les candidats plutôt
qu'en changeant la récupération ? La marge se mesure avant de construire quoi que ce soit.

| | global | paraphrase | temporal |
|---|---|---|---|
| Recall@5 | 36/49 (73 %) | 16/17 | 3/4 |
| Recall@10 | 38/49 (78 %) | 17/17 | 4/4 |
| Recall@20 | 38/49 (78 %) | 17/17 | 4/4 |

**Le plafond est exactement deux requêtes**, une paraphrase et une temporelle dont la
réponse est entre le rang six et le rang dix. Et `@20 = @10` : tout ce qui est atteignable
est déjà dans les dix premiers, donc les onze autres échecs ne sont pas un problème de
classement.

Reste à savoir quel signal les remonte. Les deux cas, mesurés :

```
paraphrase_maturite ("les tomates étaient-elles mûres ?")
  1. dense=0.6268  a_msg_tomates_varietes     <- distracteur
  2. dense=0.6215  aime_tomates
  ...
  7. dense=0.5884  tomates_vertes             <- la réponse

c_temporal_piano_actuel ("quel morceau travaille-t-il en ce moment ?")
  1. dense=0.6479  c_piano_fantaisie_premiere  2026-03-15  <- l'ancien état
  ...
  8. dense=0.5671  c_piano_gymnopédie_juin     2026-06-01  <- l'état courant
```

**Aucun signal déjà disponible ne les remonte.** Dans le premier cas l'embedder préfère
réellement les cinq distracteurs, et l'ordre final suit déjà exactement l'ordre dense :
réordonner sur le score dense ne changerait rien. Dans le second, la bonne réponse est plus
récente mais son score est inférieur de 0,08, donc un biais de récence assez fort pour
renverser ça casserait toutes les questions portant sur le passé.

Il faut donc un modèle qui lit la question et l'extrait ensemble. Mesuré, sur le vrai
modèle :

| | global | paraphrase | temporal | latence |
|---|---|---|---|---|
| sans réordonnancement | 36/49 (73 %) | 16/17 | 3/4 | 54 s pour 49 requêtes |
| **avec réordonnancement** | **38/49 (78 %)** | **17/17** | **4/4** | 65 s pour 49 requêtes |

Il atteint exactement le plafond, deux tirages sur deux, pour **environ 220 ms par
recherche**. Rien d'autre ne bouge : l'ACL, le lexical et les négatives sont inchangés.

**Il reste désactivé par défaut**, parce que c'est le seul appel de modèle du chemin de
recherche et que le critère 9 de la spec interdit d'en dépendre. L'activer est un
arbitrage entre 220 ms et cinq points de rappel, et cet arbitrage appartient au
déploiement. Une panne du modèle fait retomber sur l'ordre de la fusion sans faire échouer
la recherche.

Un défaut d'intégration trouvé au passage, par un test et non par la mesure : `FitBudget`
retrie défensivement par score décroissant pour savoir quel extrait dégrader, et annulait
donc l'ordre du réordonnanceur. Le rappel s'améliorait quand même, puisque la troncature
décide quels extraits survivent avant que le budget ne s'applique, mais l'ordre rendu
restait celui de la fusion. Le score porte maintenant l'ordre du réordonnanceur, et le
score de fusion passe dans `RawScores` sous la clé `fusion` plutôt que d'être perdu.

## Le graphe, tranché par la mesure

Avec le classement des faits par pertinence à la question en place, la question restait :
les candidats du graphe doivent-ils entrer dans la fusion ?

| | global | graphe | négative | temporal |
|---|---|---|---|---|
| graphe désactivé | 36/49 (73 %) | 1/11 | 3/4 | 3/4 |
| **faits seuls** | **36/49 (73 %)** | 1/11 | **3/4** | **3/4** |
| candidats fusionnés | 34/49 (69 %) | 3/11 | 0/4 | 2/4 |

Les candidats gagnent deux requêtes de la famille graphe et perdent trois questions sans
réponse et une temporelle. Et les deux gagnées font partie des sept que l'audit signale
comme résolubles sans le graphe : en usage normal, toutes stratégies actives, l'appelant
les aurait de toute façon. Le gain est donc illusoire et le coût réel.

`graph.fuse_candidates` est à `false` par défaut. **Le graphe alimente le `context_block`,
il ne concourt pas au classement des extraits.** C'est la conclusion de conception de toute
cette campagne, et elle est mesurée et non supposée : répondre à une question à deux sauts
demande de raisonner sur la chaîne, ce que le critère 9 interdit sur le chemin de recherche
et que le modèle lecteur fait très bien si on lui donne la matière.

## Ce qui reste à faire

1. **Le premier saut de l'extraction**, encore et toujours : la famille `graph` plafonne à
   4/8 et ses échecs sont des arêtes que le modèle n'a pas produites.
2. **La quatrième question sans réponse.** Trois sur quatre sont fermées par la détection
   ci-dessus; la dernière a un voisinage qui n'est pas plat et échappe à la conjonction.
   La fermer demanderait un signal de plus, pas un seuil de plus.
3. **Mesurer la précision, pas seulement le rappel.** Le harnais vérifie que le bon souvenir
   est dans les cinq rendus, jamais que les quatre autres sont utiles. Sur un corpus de
   272 messages c'est devenu une question qui a un sens, contrairement au précédent.
4. **Un corpus dont je ne suis pas l'auteur.** Celui-ci est écrit d'après une bible que j'ai
   rédigée, avec des questions écrites par les mêmes agents qui ont écrit les
   conversations. L'audit écarte le biais lexical le plus grossier, pas le biais de
   familiarité.

## Rejouer

```bash
source ~/.zsh_env    # LLM_API_URL, LLM_API_KEY
export POSTGRES_DSN='postgres://cinnabar:cinnabar@localhost:5433/cinnabar?sslmode=disable'
go test ./eval/                                      # valide la structure du corpus
go run ./eval -config config.yaml -k 5 -audit         # témoin, plus l'audit du corpus

# Avec le graphe: partir de config.yaml et basculer le drapeau, l'activer dans
# config.yaml lui-même imposerait un appel de modèle par message à quiconque
# lance le service.
sed 's/^\( *\)enabled: false/\1enabled: true/' config.yaml > /tmp/config-graphe.yaml
go run ./eval -config /tmp/config-graphe.yaml -k 5   # environ 7 minutes
```

Le `-keep` conserve le workspace en base, ce qui est la seule façon de savoir si un échec
de la famille `graph` vient de la traversée ou d'une arête que le modèle n'a pas extraite.
C'est comme ça que le diagnostic de la note précédente a été fait.
