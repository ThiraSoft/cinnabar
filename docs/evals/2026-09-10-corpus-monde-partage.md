# La bible du monde partagé du corpus d'évaluation

Ce document est la spécification qui a servi à écrire `eval/dataset.json`. Il est versionné
parce qu'étendre le corpus sans lui produirait des conversations qui ne ressemblent à
aucune autre, et un corpus dont chaque conversation vit dans son propre univers rend la
recherche triviale. Ce qu'on mesure, c'est la capacité à distinguer des conversations
*proches*.

Quatre lots ont été écrits en parallèle depuis cette bible, chacun avec son préfixe
d'identifiants pour éviter les collisions : `a_` pour le potager et la cuisine, `b_` pour
le travail et les ressources humaines, `c_` pour le piano, les animaux et le sport, `d_`
pour le bricolage, les voyages et les distracteurs. Le jeu d'origine, sans préfixe, est
conservé dans le même fichier.

Pour étendre le corpus, reprendre cette bible, choisir un préfixe libre, et relire
`go test ./eval/` avant de fusionner.

---


Tous les lots se passent dans le même univers, avec les mêmes personnes et les
mêmes fils en cours. C'est essentiel : un corpus dont chaque conversation vit
dans son propre univers rend la recherche triviale, parce qu'aucune conversation
ne ressemble à une autre. Ce qu'on veut mesurer, c'est la capacité à distinguer
des conversations *proches*.

## Les personnes

- **user:paul** — Paul Vasseur, 41 ans, développeur, habite Villeurbanne. Jardine
  (potager en carrés), joue du piano depuis deux ans, bricole. Allergique aux
  fruits de mer. Chat noir nommé Nocturne. A un frère, Damien, qui vit à Nîmes.
- **user:claire** — Claire Berthier, responsable de l'équipe Support, basée à
  Lyon Part-Dieu. Collègue de Paul. Court le semi-marathon.
- **user:sofia** — Sofia Nedjar, designer, l'autre voisine de bureau de Paul.
  Végétarienne. Deux chats, Pixel et Vecteur.
- **user:damien** — Damien Vasseur, le frère de Paul, viticulteur près de Nîmes.
- **user:alice** — Alice Moreau, contrôle de gestion. Ne parle jamais à Paul
  directement : elle sert aux conversations auxquelles Paul n'a pas accès.

## Les agents

`agent:jardinage`, `agent:cuisine`, `agent:bricolage`, `agent:musique`,
`agent:rh`, `agent:voyages`, `agent:animaux`, `agent:sport`, `agent:compta`,
`agent:achats`.

## Les fils en cours, à faire vivre d'un lot à l'autre

- **Le potager de Paul.** Deux carrés. Tomates (Rose de Berne, Coeur de Boeuf),
  courgettes, basilic. Il est passé d'un état laissé en friche en avril à
  impeccable en juin, puis envahi de pucerons en juillet. Un motoculteur Staub
  PP2X acheté d'occasion 320 euros.
- **Le travail.** Paul a rejoint l'équipe Support en mai, sous Claire. L'équipe
  déménage de Villeurbanne à Lyon Part-Dieu en juin. Sofia reste au design.
- **Le piano.** Paul travaille la Fantaisie-Impromptu de Chopin, puis passe à une
  Gymnopédie de Satie en juin parce que la première le décourage.
- **Le voyage.** Randonnée dans les Cévennes avec Damien en août, gorges du Tarn,
  puis annulée et remplacée par le Vercors.
- **Les animaux.** Nocturne, le chat de Paul, dort le jour. Les chats de Sofia,
  Pixel et Vecteur. Une visite chez le vétérinaire en juillet.
- **Le bricolage.** Une perceuse Bosch GSR 18V-60 FC. Une étagère montée en juin,
  une porte de placard retapée en juillet.

## Les règles d'écriture

1. **Français naturel, ton de vraie conversation.** Des phrases inégales, des
   digressions, des « bon », des « du coup ». Pas de style catalogue.
2. **Des voisinages sémantiques volontaires.** Écrire des conversations qui
   *ressemblent* à la conversation cible sans contenir la réponse : parler de
   tomates ailleurs que là où se trouve le fait cherché, parler de déménagement
   dans un autre contexte que celui de l'équipe. C'est ce qui rend la mesure
   sérieuse.
3. **Dater les messages.** Chaque message porte un `created_at` en RFC3339, entre
   `2026-03-01T08:00:00Z` et `2026-08-31T20:00:00Z`, croissant à l'intérieur
   d'une conversation, et cohérent avec la chronologie des fils ci-dessus.
4. **Un fait n'est énoncé qu'une fois.** Si un fait cherché par une requête est
   répété dans deux conversations, la requête cesse de mesurer quoi que ce soit.
5. **Les identifiants `id` ne sont posés que sur les messages qu'une requête
   cible.** Les autres n'en ont pas. Un `id` est unique dans tout le corpus.
