-- Un label peut être réutilisé après révocation, mais jamais porté par deux
-- clients vivants en même temps: sans cette contrainte, `keys create` peut
-- créer un second client sous un label déjà utilisé sans avertissement, et
-- `keys revoke <label>` révoquerait alors les deux à la fois plutôt que
-- d'aider l'opérateur à distinguer lequel il visait.
CREATE UNIQUE INDEX api_clients_label_active_idx
    ON api_clients (label) WHERE revoked_at IS NULL;
