# Cinnabar face à MemPalace, sur leurs propres bancs

MemPalace (github.com/MemPalace/mempalace) publie ses chiffres sur LoCoMo,
LongMemEval, ConvoMem et MemBench, avec les scripts qui les produisent. Cette
note mesure Cinnabar sur les mêmes données, découpées comme dans leurs scripts
et notées avec leurs métriques, et en tire les réglages par défaut sans modèle
de langage.

Seul LoCoMo est complet à ce jour. Les trois autres bancs suivront dans cette
note.

## Conditions

| | |
|---|---|
| Cinnabar | `nomic-embed-text-v2-moe` en processus (golem, Vulkan), PostgreSQL 16 + pgvector |
| MemPalace | leurs scripts, ChromaDB 1.5.7, `all-MiniLM-L6-v2` (leur défaut) |
| Modèle, rerank et graphe | `gemma-4-12B-it-QAT-Q4_0` sur golem-server, 4 slots |
| LoCoMo | 10 conversations, 5 882 messages, 1 986 questions, catégorie 5 comprise comme chez eux |
| Harnais | `eval/mempalace` |

Leurs chiffres ont été reproduits chez nous avant toute comparaison : 60,3 %
en session vectorielle à R@10 (publié : 60,3 %), 89,2 % en hybride (publié :
88,9 %), 47,9 % en tours de dialogue (publié : 48,0 %).

Deux granularités, parce que les deux systèmes ne rendent pas la même chose.
MemPalace en mode session rend des sessions entières, environ 750 tokens
chacune. Cinnabar rend des extraits : un noyau de messages contigus et leurs
voisins. « Session » compte les sessions d'où viennent les messages rendus,
« dialogue » les tours de preuve rendus.

## LoCoMo

| Système | Session R@5 | Session R@10 | Dialogue R@5 | Dialogue R@10 | Tokens à k=10 |
|---|---|---|---|---|---|
| MemPalace vectoriel | 46,2 % | 60,3 % | 38,9 % | 47,9 % | ~7 500 |
| MemPalace hybride | 78,4 % | 89,2 % | 50,2 % | 58,6 % | ~7 500 |
| Cinnabar, config d'avant | 74,6 % | 78,9 % | 72,3 % | 76,2 % | 1 877 |
| Cinnabar sans plancher ni non-réponse | 85,0 % | 91,4 % | 82,4 % | 88,0 % | 2 370 |
| Cinnabar + rerank | 89,1 % | 92,1 % | 86,9 % | 88,7 % | 2 372 |
| Cinnabar + graphe dans la fusion | 79,5 % | 89,4 % | 76,6 % | 85,9 % | 2 787 |
| Cinnabar + graphe, sources des faits comprises | 89,3 % | 93,3 % | 79,6 % | 87,1 % | 2 787 |
| Cinnabar + graphe + rerank, faits compris | 92,8 % | 95,0 % | 86,3 % | 90,3 % | 2 814 |

Sans modèle, Cinnabar dépasse leur meilleur mode avec trois fois moins de
texte rendu. La ligne « dialogue » ne compare pas des unités de même taille
(un extrait regroupe plusieurs messages), la ligne « session » si, et elle
est à l'avantage de Cinnabar à volume plus faible.

Le rerank de MemPalace ne se compare que sur deux conversations (conv-26 et
conv-30, 304 questions). Leur script exige une clé Anthropic même avec un
endpoint local, et laissait le modèle répondre 1 024 tokens : il a fallu
passer une clé factice et borner la réponse à 24 tokens. Il gagne 0,3 point à
R@5 et rien à R@10, parce qu'il ne remonte qu'un document en tête. Sur ces
deux conversations, Cinnabar avec rerank fait 91,4 % à R@5 contre 84,9 %, et
94,4 % à R@10 contre 95,7 %, sur une conversation de 19 sessions où leur
top-10 couvre la moitié du corpus.

Ce que disent les variantes :

- Le rerank paie à k=5 (+4 points), presque plus à k=10. Contraint par un
  schéma JSON, il a échoué 13 fois sur 7 900 appels.
- Les candidats du graphe dans la fusion dégradent le classement (-5 points
  à k=5), ce qui confirme `fuse_candidates: false`. Les faits eux-mêmes,
  rendus dans le `context_block`, apportent +10 points à k=5.
- L'extraction par fenêtre de messages (branche `graphe-par-fenetre`) a
  construit le graphe de LoCoMo en 1 h 09 au lieu des 3 h 20 que coûtait un
  appel par message, avec 9,4 messages par appel et aucune fenêtre perdue.

## Réglages sans modèle

Balayage sur LoCoMo, une seule ingestion, `-sweep` du harnais.

| Réglage | Effet sur le rappel dialogue à k=5 |
|---|---|
| Détection de non-réponse (0,58 ; 0,05) | -9,7 points |
| Plancher dense 0,52 | -3,5 points |
| Plancher dense 0,45 et en dessous | -0,2 point ou moins |
| `rrf_k` 20 ou 60, `dense_top_k` 20 ou 40 | moins de 0,5 point |
| Élargissement 3 au lieu de 2 | +0,1 point pour 18 % de tokens en plus |

| k | budget 1 200 | budget 2 000 | sans budget |
|---|---|---|---|
| 5 | 81,0 % | 82,4 % | 82,4 % |
| 8 | 82,7 % | 86,2 % | 86,8 % |
| 10 | 82,7 % | 86,8 % | 88,0 % |

### La détection de non-réponse ne se transpose pas

Sur le corpus français, les quatre questions sans réponse ont un meilleur
score dense entre 0,49 et 0,56. Sur LoCoMo, 10 % des questions qui ont une
réponse sont sous 0,554. Aucun couple de seuils ne sépare les deux :

| Seuils (meilleur ; marge) | Questions LoCoMo écartées (dont trouvées sans détection) | Négatives françaises fermées |
|---|---|---|
| 0,50 ; 0,03 | 28 (18) | 1/4 |
| 0,54 ; 0,07 | 133 (89) | 3/4 |
| 0,58 ; 0,05 (défaut d'avant) | 296 (203) | 3/4 |
| 0,58 ; 0,07 | 326 (231) | 4/4 |

Une similarité cosinus absolue dépend de la langue et du corpus. La détection
reste disponible, à calibrer sur son corpus, et passe désactivée par défaut.

### Nouveaux défauts

`final_top_k` 8, `max_memory_tokens` 2 000, `minimum_dense_score` 0,45,
détection de non-réponse désactivée, `rerank.pool` 16.

| Corpus | Avant | Après |
|---|---|---|
| LoCoMo, dialogue | 72,3 % (k=5, sans budget) | 86,2 % (k=8, budget 2 000) |
| Corpus français, questions avec réponse | 33/45 | 35/45 |
| Corpus français, questions sans réponse fermées | 3/4 | 0/4 |

## Reproduire

```bash
go run ./eval/mempalace -bench locomo -data locomo10.json -config bench.yaml \
    -out locomo.jsonl -graph            # variantes base, rerank, graphe
go run ./eval/mempalace -bench locomo -data locomo10.json -config bench.yaml \
    -out sweep.jsonl -sweep -tag _sweep # balayage sans modèle
go run ./eval/mempalace -report -out locomo.jsonl
```

Côté MemPalace : `benchmarks/locomo_bench.py --top-k 50`, puis R@5 et R@10
recalculés sur le classement complet, leur rappel à 50 étant structurellement
de 100 % sur des conversations de 19 à 32 sessions.
