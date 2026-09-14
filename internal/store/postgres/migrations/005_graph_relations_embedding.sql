-- Le classement de la stratégie graphe n'avait aucun signal de pertinence par
-- rapport à la question posée: il triait par nombre de sauts, puis confiance,
-- puis récence. Mesuré sur le corpus d'évaluation, trois questions sans aucun
-- rapport entre elles rendaient exactement les mêmes quatorze faits, ceux du
-- voisinage de l'entité graine, parce que rien ne permettait de préférer
-- celui qui répond.
--
-- On embarque donc une représentation vectorielle du fait rendu en phrase, à
-- l'écriture, et la recherche classe par distance cosinus à la question, comme
-- le fait déjà la stratégie dense sur les unités de mémoire. Même modèle, même
-- dimension, même préfixe document.
--
-- La colonne est nullable et la recherche retombe sur l'ordre précédent quand
-- elle est vide: une relation écrite avant cette migration, ou écrite pendant
-- une panne de l'embedder, reste trouvable.
ALTER TABLE graph_relations ADD COLUMN embedding vector(768);

-- Même famille d'index et même opérateur que memory_units_embedding_idx: le
-- classement se fait par distance cosinus. L'index est partiel, la majorité
-- des lignes pouvant être sans embedding sur une base existante.
CREATE INDEX graph_relations_embedding_idx
    ON graph_relations USING hnsw (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;
