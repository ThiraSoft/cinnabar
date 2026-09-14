package memory

// Stats porte les agrégats exposés par /debug/stats. Un type nommé plutôt
// qu'un map[string]any rend structurel l'engagement de n'exposer que des
// agrégats: un champ message, identité ou conversation_id ne peut pas s'y
// glisser sans modifier ce type, et donc sans que ça se voie en revue.
// Déclaré ici, dans le domaine, plutôt que dans internal/api ou
// internal/store/postgres: les deux couches le produisent ou le
// consomment, et aucune des deux ne doit dépendre de l'autre.
type Stats struct {
	QueueDepth    map[string]QueueDepth `json:"queue_depth"`
	DeadJobs      int                   `json:"dead_jobs"`
	MemoryUnits   int                   `json:"memory_units"`
	Messages      int                   `json:"messages"`
	Conversations int                   `json:"conversations"`
	DBPool        DBPoolStats           `json:"db_pool"`
}

// QueueDepth compte, pour un type de job, les jobs en attente (pending) et en
// lettre morte (dead), séparément: une file saine a un pending qui varie
// librement et un dead qui reste à zéro, ce ne sont pas deux nombres que
// l'on veut voir fondus en un seul.
type QueueDepth struct {
	Pending int `json:"pending"`
	Dead    int `json:"dead"`
}

// DBPoolStats reflète l'état du pool de connexions PostgreSQL, utile pour
// diagnostiquer une saturation sans exposer le DSN lui-même.
type DBPoolStats struct {
	Acquired int32 `json:"acquired"`
	Idle     int32 `json:"idle"`
	Total    int32 `json:"total"`
}
