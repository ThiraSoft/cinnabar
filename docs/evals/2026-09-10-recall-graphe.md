# Évaluation du rappel avec le graphe, mesurée contre le vrai extracteur

> **Les chiffres de cette note sont périmés.** Ils portent sur un corpus de 20 messages,
> plus petit qu'une page de résultats, et leur 100 % ne mesurait pas grand-chose. La mesure
> qui fait foi est dans `2026-09-10-recall-corpus-etendu.md`, sur 272 messages, et elle
> corrige notamment la conclusion sur le plateau du plancher de pertinence, qui était un
> artefact de taille. Ce qui reste valable ici est le récit des quatre défauts trouvés et
> de la façon dont ils l'ont été.


Cette note remplace la version qui documentait l'impossibilité de mesurer. Le flux vers
le modèle d'extraction a été ouvert, et ce qui a suivi a fait passer le Recall@5 de 79 %
à **100 %** sur ce corpus, en trois corrections dont aucune ne touche au SQL de recherche.

| Étape | graphe désactivé | graphe activé |
|---|---|---|
| Point de départ | 79 % | 64-71 % |
| Prompt d'extraction v2 | 79 % | 86 % |
| Plus le plancher de pertinence dense | 93 % | 93 % |
| Plus le filtre d'alias | **93 %** | **100 %** |

Le chemin est plus instructif que le chiffre. Il contient une conclusion que j'avais tirée
trop vite, deux fonctionnalités qui ne se déclenchaient jamais, et un pronom.

Référence de comparaison : `docs/evals/2026-09-10-recall-baseline.md`,
la mesure de la v1, sans graphe.

## Conditions

| | |
|---|---|
| Extracteur | `google/gemma-4-31B-it` via une API compatible OpenAI, `${LLM_API_URL}`, contexte 131072 |
| Embeddings | `nomic-embed-text-v2-moe` sur Ollama local, 768 dimensions |
| Corpus | 10 conversations, 20 messages, 14 requêtes annotées en 5 familles |
| Profondeur | Recall@5, `final_top_k` à 5 |
| Graphe extrait | 27 entités, 15 relations, sur 20 messages ingérés |

Le schéma JSON contraint est respecté par le modèle sur les 20 appels, sans un seul
échec de parsing. Les types d'entités reviennent en français (`person`, `location`,
`plantcultivar`), ce que la clé canonique met en minuscules de toute façon.

## Les chiffres

| Configuration | paraphrase | lexical | graphe | négative | acl | global |
|---|---|---|---|---|---|---|
| Graphe désactivé | 6/6 | 4/4 | 1/2 | 0/1 | 0/1 | 11/14 (79 %) |
| Prompt v1, `graph_top_k: 20` | 4/6 | 4/4 | 2/2 | 0/1 | 0/1 | 10/14 (71 %) |
| Prompt v1, idem, second tirage | 4/6 | 4/4 | 1/2 | 0/1 | 0/1 | 9/14 (64 %) |
| Prompt v1, `graph_top_k: 3` | 6/6 | 4/4 | 1/2 | 0/1 | 0/1 | 11/14 (79 %) |
| Prompt v1, `graph_top_k: 5` | 3/6 | 4/4 | 2/2 | 0/1 | 0/1 | 9/14 (64 %) |
| Prompt v1, `graph_top_k: 8` | 4/6 | 4/4 | 1/2 | 0/1 | 0/1 | 9/14 (64 %) |
| Prompt v2, `graph_top_k: 20` | 6/6 | 4/4 | 2/2 | 0/1 | 0/1 | 12/14 (86 %) |
| Prompt v2, idem, 3 tirages de plus | 6/6 | 4/4 | 2/2 | 0/1 | 0/1 | 12/14 (86 %) |
| Prompt v2, `graph_top_k: 3` | 6/6 | 4/4 | 2/2 | 0/1 | 0/1 | 12/14 (86 %) |
| **Configuration livrée, graphe désactivé** | 6/6 | 4/4 | 1/2 | **1/1** | **1/1** | **13/14 (93 %)** |
| **Configuration livrée, graphe activé** | 6/6 | 4/4 | **2/2** | **1/1** | **1/1** | **14/14 (100 %)** |

Les deux dernières lignes sont l'état livré, trois tirages consécutifs chacune. La
différence avec les précédentes tient au plancher de pertinence dense et au filtre
d'alias, expliqués plus bas. `negative` et `acl` échouaient depuis la v1.

La ligne de la v1 (paraphrase 6/6, lexical 4/4, négative 0/1, acl 0/1) est retrouvée
à l'identique graphe désactivé, sur un corpus pourtant élargi de deux conversations :
la couche graphe n'a rien cassé de ce qui existait.

## Ce que le prompt v1 faisait, et pourquoi j'ai eu tort de conclure trop vite

Avec le prompt d'origine, activer le graphe faisait perdre deux requêtes de paraphrase de
façon reproductible, et abaisser `graph_top_k` les rendait. J'en ai conclu que les
candidats du graphe évinçaient des réponses justes parce qu'ils sont classés par nombre de
sauts, confiance et récence, trois grandeurs qui ne mesurent aucune pertinence par rapport
à la question. L'explication était cohérente, elle collait aux chiffres, et elle était
fausse.

Le même corpus, la même fusion et le même `graph_top_k: 20` donnent 86 % avec le prompt
v2. Ce qui inondait la fusion n'était pas le nombre de candidats, c'était leur inutilité.
Un graphe pauvre en relations pertinentes apporte du bruit avec de bons rangs ; un graphe
correct n'a pas besoin qu'on le brime. La leçon est moins sur le service que sur la
méthode : j'ai tiré une conclusion de conception d'une mesure qui n'avait fait varier
qu'un paramètre, alors que la variable dominante était ailleurs.

## Ce que le prompt v1 manquait

Le diagnostic n'a été possible que grâce à `-keep`, qui conserve le workspace. La
conversation contient deux messages :

> « Je viens de rejoindre l'équipe Support, sous la responsabilité de Claire. »
> « L'équipe Support est basée à Lyon Part-Dieu, dans un immeuble récent. »

Le prompt v1 produisait :

```
Support | est_basée_à     | Lyon Part-Dieu
Support | est_située_dans | immeuble récent
```

Rien ne reliait Paul à l'équipe. La chaîne n'avait qu'une arête au lieu de deux, donc
aucune graine sur Paul ne pouvait atteindre Lyon. La traversée faisait ce qu'elle devait :
c'est la matière première qui manquait.

Trois symptômes de la même cause, tous mesurés sur le workspace conservé :

| | prompt v1 | prompt v2 |
|---|---|---|
| Entités nommées « paul » | 7 | 2, dont « frère de Paul » |
| Prédicats distincts | 11 pour 14 relations | 14 pour 26 relations |
| Paul relié à son équipe | non | `paul appartient_a Support` |
| `a_pour_état` / `a_pour_etat` | deux prédicats pour un sens | un seul, sans accent |

Le v1 renvoyait `resolved: false` pour presque tout, d'où sept « paul » distincts que le
service ne fusionnera jamais de sa propre initiative, conformément à la règle 7.3. Et il
forgeait un prédicat par relation, dont deux variantes accentuées du même sens.

## Ce que le prompt v2 change

Quatre instructions, aucune ligne de SQL touchée :

1. **dire explicitement qu'une phrase reliant deux choses nommées doit produire une
   relation entre les deux entités**, avec l'exemple de Paul et de l'équipe Support. C'est
   le point qui débloque la requête à deux sauts. « N'invente rien, ne déduis rien » avait
   visiblement été lu comme une interdiction de relier ;
2. **préférer une cible entité à un littéral** quand la cible est une chose nommée ;
3. **un vocabulaire de prédicats semi-fermé** de quinze termes sans accent, avec repli
   libre. C'est ce qui stabilise le graphe et rend `single_valued_relations` utilisable ;
4. **redéfinir `resolved`** : vrai dès que l'entité est une chose nommée identifiable,
   faux seulement pour une référence réellement ambiguë. Le v1 invitait au doute.

## Le bug que cette mesure a révélé, et qui n'était pas une question de rappel

`single_valued_relations` valait `["has_observed_state"]` par défaut, un identifiant
anglais, pendant que le prompt était en français et que le modèle produisait `a_pour_etat`.
Le nom configuré n'apparaissant jamais dans les relations extraites, **tout le chaînage
temporel ne se déclenchait jamais** : la fermeture des fenêtres de validité, le recalcul
complet des chaînes, et les deux correctifs de concurrence qu'ils ont coûtés. Aucune
erreur, aucun test rouge, une fonctionnalité simplement morte.

Le défaut est désormais `a_pour_etat`, et `TestSingleValuedDefaultsExistentDansLePrompt`
garde l'accord entre les deux fichiers, en échouant exactement sur l'ancienne valeur.

Reste que le chaînage n'a toujours pas tourné de bout en bout contre une extraction
réelle : le corpus d'évaluation donne le même horodatage à tous ses messages, et deux
observations simultanées ne se ferment délibérément pas l'une sur l'autre. Le mesurer
demande un corpus daté.

## Le plancher de pertinence, enfin posé au bon endroit

Deux familles échouaient depuis la v1, et pour la même raison : `hors_sujet`
(« Quelle est la capitale de la Mongolie ? ») et `acl_budget` (le contenu d'une
conversation où le demandeur n'est pas participant) attendent **zéro résultat**, et le
service en rendait huit. L'ACL faisait son travail dans le second cas, aucun résultat ne
venait de la conversation interdite : ce qui manquait était un plancher de pertinence.

La ligne de base de la v1 avait établi que `minimum_score`, posé sur le score fusionné
par RRF, ne peut pas fonctionner, et supposé qu'un plancher sur le score dense brut le
pourrait. La mesure confirme la première moitié et corrige la seconde.

```
scores fusionnés,    pertinents : [0.0161 ... 0.0491]   non pertinents : [0.0147 ... 0.0315]
scores denses bruts, pertinents : [0.4859 ... 0.6685]   non pertinents : [0.3071 ... 0.6214]
```

Les deux distributions se chevauchent, y compris celle du dense. **Un plancher par
résultat ne sépare donc rien.** Ce qui sépare, c'est le meilleur score dense **par
requête** :

| Requête | meilleur score dense |
|---|---|
| `hors_sujet` (rien ne doit répondre) | 0,4655 |
| `acl_budget` (rien ne doit répondre) | 0,4777 |
| `terme_rare_cultivar` (réussit par le lexical) | 0,4859 |
| `graph_deux_sauts` (réussit par le graphe) | 0,0000 |
| les dix autres | 0,5283 à 0,6685 |

La marge entre les deux familles négatives et la plus faible des réussies est de 0,008.
Choisir un seuil là serait de la superstition, et c'est ce qui m'a d'abord fait renoncer.

Ce qui débloque est ailleurs : **le plancher ne doit filtrer que les candidats du dense.**
Une correspondance lexicale est une preuve de pertinence par elle-même, un fait du graphe
aussi. `terme_rare_cultivar` survit donc à un plancher bien au-dessus de son propre score
dense, parce que c'est le lexical qui la porte, et `graph_deux_sauts` survit avec un score
dense nul. Le plateau mesuré est alors large :

| Plancher | Recall@5 |
|---|---|
| 0,45 | 12/14, les deux négatives fuient encore |
| **0,50 à 0,55** | **14/14** |
| 0,60 | 13/14, une vraie réponse est perdue |

Le défaut livré est 0,52, le centre du plateau. C'est une calibration sur quatorze
requêtes et un seul modèle d'embedding : elle est à refaire par corpus, et `null` restaure
le comportement d'avant, qui répond toujours quelque chose.

## Un pronom rendait la dernière question insoluble

Avec le plancher, `acl_budget` se fermait mais pas `hors_sujet` : dense à 0, lexical à 0,
et **douze candidats du graphe**. Les compteurs de diagnostic, ajoutés au harnais pour
l'occasion, ont pointé la stratégie ; le reste s'est trouvé en base.

L'entité « tomates » portait l'alias **« elles »**. Et
`word_similarity('elles', 'Quelle est la capitale de la Mongolie ?')` vaut 0,429,
au-dessus du seuil de détection de graines, sur le trigramme commun de « elles » et
« Quelle ». Une question sur la Mongolie semait donc le graphe sur les tomates de Paul, et
en rendait douze relations avec d'excellents rangs.

Deux défauts se composaient : le modèle rangeait une reprise pronominale parmi les alias,
et un jeton court et fréquent collisionne facilement en trigrammes. Le correctif tient sur
les deux : le prompt dit désormais qu'un alias est un autre nom de la chose et jamais un
pronom, et `cleanAliases` écarte pronoms, déterminants et jetons d'un seul caractère avant
l'écriture, tout en gardant les sigles courts qui sont de vrais alias.

## Ce qu'il reste à faire

1. **Un corpus daté**, pour que le chaînage temporel tourne enfin de bout en bout contre
   une extraction réelle. C'est la seule partie du graphe qui n'a jamais été exercée
   autrement que sur des jeux montés à la main.
2. **Un corpus plus large.** Quatorze requêtes suffisent à voir un effet grossier, pas à
   régler quoi que ce soit : les mesures à `graph_top_k` variable du prompt v1 ne sont pas
   monotones, ce qui est la signature du bruit.
3. **Les deux familles qui échouent depuis la v1**, `negative` et `acl`, restent à
   0/1 chacune et n'ont rien à voir avec le graphe. La première demande un plancher de
   pertinence, dont la ligne de base a montré qu'il ne peut pas porter sur le score fusionné.

## Rejouer la mesure

```bash
source ~/.zsh_env                 # définit LLM_API_URL et LLM_API_KEY
export POSTGRES_DSN='postgres://cinnabar:cinnabar@localhost:5433/cinnabar?sslmode=disable'
go run ./eval -config config.yaml -k 5          # témoin, graphe désactivé
go run ./eval -config config-graph.yaml -k 5    # graphe activé
go run ./eval -config config-graph.yaml -k 5 -keep   # conserve le workspace
```

`-keep` est ce qui permet le diagnostic du second constat : sans lui le workspace est
supprimé et on ne peut plus savoir si l'échec vient de la traversée ou de l'extraction.
Le token du LLM peut expirer ; `eval` refuse de démarrer avec un message
nommant la variable manquante plutôt qu'en échouant au premier appel.
