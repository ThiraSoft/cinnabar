// Package config charge la configuration du service depuis un fichier YAML.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Service struct {
	Listen             string        `yaml:"listen"`
	MaxRequestBytes    int64         `yaml:"max_request_bytes"`
	ConsistencyDefault string        `yaml:"consistency_default"`
	DebugSearch        bool          `yaml:"debug_search"`
	ReadHeaderTimeout  time.Duration `yaml:"read_header_timeout"`
	ReadTimeout        time.Duration `yaml:"read_timeout"`
	WriteTimeout       time.Duration `yaml:"write_timeout"`
	ShutdownGrace      time.Duration `yaml:"shutdown_grace"`

	// MaxCandidateLimit, MaxResultLimit et MaxTokenBudget bornent les
	// équivalents fournis par l'appelant dans une requête de recherche
	// (candidate_limit, result_limit, token_budget): un coût de requête
	// fourni par l'appelant doit être plafonné à la frontière, pas transmis
	// tel quel au planificateur de requêtes, sous peine qu'un tenant en
	// dégrade un autre sur un service partagé.
	MaxCandidateLimit int `yaml:"max_candidate_limit"`
	MaxResultLimit    int `yaml:"max_result_limit"`
	MaxTokenBudget    int `yaml:"max_token_budget"`
}

type Database struct {
	DSN             string        `yaml:"dsn"`
	MaxConns        int32         `yaml:"max_conns"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"`
}

// Fournisseurs et devices d'embedding reconnus.
const (
	EmbeddingProviderHTTP  = "http"
	EmbeddingProviderGolem = "golem"
	EmbeddingDeviceCPU     = "cpu"
	EmbeddingDeviceVulkan  = "vulkan"
)

type Embedding struct {
	// Provider vaut "http" (endpoint OpenAI-compatible, base_url) ou "golem"
	// (inférence dans le processus, model_path et device).
	Provider       string        `yaml:"provider"`
	ModelPath      string        `yaml:"model_path"`
	Device         string        `yaml:"device"`
	BaseURL        string        `yaml:"base_url"`
	Model          string        `yaml:"model"`
	Dimensions     int           `yaml:"dimensions"`
	DocumentPrefix string        `yaml:"document_prefix"`
	QueryPrefix    string        `yaml:"query_prefix"`
	Normalize      bool          `yaml:"normalize"`
	Timeout        time.Duration `yaml:"timeout"`
}

type Extraction struct {
	BaseURL string            `yaml:"base_url"`
	Model   string            `yaml:"model"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`
	Timeout time.Duration     `yaml:"timeout"`
	// MaxTokens borne la réponse du modèle. Un serveur qui applique son
	// propre défaut (1024 jetons chez golem) coupe une extraction indentée
	// au milieu de son JSON. Zéro laisse le serveur décider.
	MaxTokens int `yaml:"max_tokens"`
}

type Indexing struct {
	Strategy            string `yaml:"strategy"`
	PreviousMessages    int    `yaml:"previous_messages"`
	MaxChars            int    `yaml:"max_chars"`
	CharsPerToken       int    `yaml:"chars_per_token"`
	IndexUserMessages   bool   `yaml:"index_user_messages"`
	IndexAgentMessages  bool   `yaml:"index_agent_messages"`
	IndexSystemMessages bool   `yaml:"index_system_messages"`
	IndexToolMessages   bool   `yaml:"index_tool_messages"`
	Version             int    `yaml:"version"`
}

type Retrieval struct {
	DenseTopK       int      `yaml:"dense_top_k"`
	LexicalTopK     int      `yaml:"lexical_top_k"`
	GraphTopK       int      `yaml:"graph_top_k"`
	FinalTopK       int      `yaml:"final_top_k"`
	MaxMemoryTokens int      `yaml:"max_memory_tokens"`
	ExpandBefore    int      `yaml:"expand_before"`
	ExpandAfter     int      `yaml:"expand_after"`
	RRFK            int      `yaml:"rrf_k"`
	MinimumScore    *float64 `yaml:"minimum_score"`

	// MinimumDenseScore est un plancher de pertinence appliqué au score
	// dense brut, avant la fusion. C'est la seule des deux grandeurs sur
	// laquelle un plancher puisse porter: une similarité cosinus garde son
	// sens hors de son classement, alors que le score fusionné par RRF
	// n'encode qu'un rang (voir MinimumScore et la section 12 de la spec).
	//
	// Il ne filtre que les candidats du dense. Une correspondance lexicale
	// reste une preuve de pertinence quelle que soit la distance cosinus, et
	// c'est voulu: sur le corpus d'évaluation, une requête à terme rare
	// réussit avec un meilleur score dense de 0,486, plus bas que celui de
	// deux requêtes auxquelles rien ne devait répondre.
	//
	// nil désactive le plancher, et c'est le défaut. Le calibrer demande un
	// corpus réel: voir docs/evals/ pour les distributions
	// mesurées et la marge, qui est trop mince sur le corpus d'évaluation
	// pour qu'une valeur y soit choisie honnêtement.
	MinimumDenseScore *float64 `yaml:"minimum_dense_score"`

	// NoAnswerBestBelow et NoAnswerMarginBelow détectent ensemble une
	// question à laquelle rien ne répond dans ce workspace. Les deux
	// conditions doivent être réunies pour que les candidats du dense
	// soient écartés, et c'est le point du dispositif: la conjonction rend
	// la règle conservatrice.
	//
	// La mesure qui la justifie est dans docs/evals/. Ni la
	// similarité absolue ni la marge ne séparent seules: la requête de
	// paraphrase qui réussit avec la plus faible marge du corpus (0,027) est
	// plus resserrée que les quatre questions sans réponse, mais son
	// meilleur candidat est à 0,627 quand les leurs plafonnent à 0,557. Une
	// question qui a une réponse présente donc au moins l'un des deux
	// signes: quelque chose de vraiment proche, ou quelque chose qui se
	// détache. N'écarter que lorsque les deux manquent.
	//
	// nil sur l'un des deux désactive la détection.
	NoAnswerBestBelow   *float64 `yaml:"no_answer_best_below"`
	NoAnswerMarginBelow *float64 `yaml:"no_answer_margin_below"`
}

type Graph struct {
	Enabled bool `yaml:"enabled"`

	// FuseCandidates dit si la stratégie graphe contribue des candidats à
	// la fusion RRF, en plus des faits qu'elle met dans le context_block.
	//
	// La question n'est pas rhétorique et elle est mesurée. Un candidat du
	// graphe est un message source d'une relation, classé par nombre de
	// sauts puis confiance puis récence: aucune de ces grandeurs ne mesure
	// la pertinence par rapport à la question. Trois questions sans rapport
	// posées au même corpus rendent d'ailleurs exactement les mêmes
	// candidats, parce qu'elles nomment toutes Paul et que le graphe rend
	// alors tout ce qu'il sait de lui.
	//
	// Les faits, eux, vont dans le context_block, où le modèle lecteur peut
	// raisonner sur la chaîne. C'est là que la valeur du graphe se trouve,
	// puisque répondre à une question à deux sauts demande un raisonnement
	// que le critère 9 interdit sur le chemin de recherche.
	FuseCandidates        bool     `yaml:"fuse_candidates"`
	ContextMessages       int      `yaml:"context_messages"`
	MaxHops               int      `yaml:"max_hops"`
	SingleValuedRelations []string `yaml:"single_valued_relations"`
}

// Rerank configure le réordonnancement des extraits par un modèle qui lit la
// question et les extraits ensemble.
//
// Désactivé par défaut, et pas par prudence de façade: c'est le seul appel de
// modèle du chemin de recherche, ce que le critère 9 de la spec interdit.
// L'activer est un arbitrage entre la latence d'un appel de modèle par
// recherche et le rappel qu'il fait gagner. La mesure est dans
// docs/evals/.
type Rerank struct {
	Enabled bool `yaml:"enabled"`

	// Pool est le nombre d'extraits soumis au modèle. Il doit dépasser
	// final_top_k, sinon il n'y a rien à réordonner. Mesuré: tout ce qui
	// est atteignable sur le corpus d'évaluation tient dans les dix
	// premiers, donc au-delà de deux fois final_top_k il n'y a plus de
	// marge, seulement du coût.
	Pool int `yaml:"pool"`

	BaseURL string            `yaml:"base_url"`
	Model   string            `yaml:"model"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`
	Timeout time.Duration     `yaml:"timeout"`
}

type Access struct {
	DefaultScope string `yaml:"default_scope"`
}

type Jobs struct {
	Workers      int           `yaml:"workers"`
	RetryLimit   int           `yaml:"retry_limit"`
	PollInterval time.Duration `yaml:"poll_interval"`

	// ReclaimAfter est l'ancienneté, pour un job resté en running, au-delà
	// de laquelle son worker est considéré mort (OOM kill, drain de noeud,
	// SIGKILL pendant un déploiement) et le job repris. validate() refuse
	// toute valeur sous JobHandlerTimeout*2: un handler lent mais vivant ne
	// doit jamais se faire voler son job.
	ReclaimAfter time.Duration `yaml:"reclaim_after"`
}

// JobHandlerTimeout borne le temps qu'un handler de job a pour traiter un
// job avant que son contexte ne soit annulé (internal/jobs.Runner.handle
// l'utilise pour dériver ce contexte). Déclarée ici plutôt que dans
// internal/jobs: internal/jobs importe déjà internal/config, donc c'est le
// seul sens qui évite un cycle d'import, et c'est aussi ici que
// Jobs.ReclaimAfter doit rester au moins le double de cette valeur, pour
// qu'un handler lent mais vivant ne se fasse jamais voler son job par une
// reprise trop agressive.
const JobHandlerTimeout = 5 * time.Minute

type Config struct {
	Service    Service    `yaml:"service"`
	Database   Database   `yaml:"database"`
	Embedding  Embedding  `yaml:"embedding"`
	Extraction Extraction `yaml:"extraction"`
	Indexing   Indexing   `yaml:"indexing"`
	Retrieval  Retrieval  `yaml:"retrieval"`
	Graph      Graph      `yaml:"graph"`
	Rerank     Rerank     `yaml:"rerank"`
	Access     Access     `yaml:"access"`
	Jobs       Jobs       `yaml:"jobs"`
}

func defaults() Config {
	return Config{
		Service: Service{
			Listen: ":8080", MaxRequestBytes: 1 << 20,
			ConsistencyDefault: "eventual", DebugSearch: false,
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 60 * time.Second, ShutdownGrace: 30 * time.Second,
			MaxCandidateLimit: 500, MaxResultLimit: 50, MaxTokenBudget: 20000,
		},
		Database: Database{MaxConns: 10, MaxConnLifetime: 30 * time.Minute},
		Embedding: Embedding{
			Provider: EmbeddingProviderHTTP, Device: EmbeddingDeviceVulkan,
			BaseURL: "http://localhost:11434/v1", Model: "nomic-embed-text-v2-moe",
			Dimensions: 768, DocumentPrefix: "search_document: ",
			QueryPrefix: "search_query: ", Normalize: true, Timeout: 30 * time.Second,
		},
		Extraction: Extraction{Timeout: 120 * time.Second, MaxTokens: 4096},
		Indexing: Indexing{
			Strategy: "contextualized_message", PreviousMessages: 2,
			MaxChars: 1600, CharsPerToken: 4,
			IndexUserMessages: true, IndexAgentMessages: true,
			IndexSystemMessages: false, IndexToolMessages: false, Version: 1,
		},
		Retrieval: Retrieval{
			DenseTopK: 20, LexicalTopK: 20, GraphTopK: 20, FinalTopK: 5,
			MaxMemoryTokens: 1200, ExpandBefore: 2, ExpandAfter: 2, RRFK: 60,
			// Le plancher de pertinence est actif par défaut, contrairement
			// à MinimumScore qui reste nil parce qu'il porte sur la mauvaise
			// grandeur. Sans lui, une question à laquelle rien du workspace
			// ne répond rend quand même final_top_k souvenirs, que
			// l'appelant injectera dans un prompt: le dense n'a pas de
			// plancher naturel, il rend ses voisins les plus proches aussi
			// loin soient-ils.
			//
			// 0,52 est le centre du plateau mesuré sur le corpus
			// d'évaluation, où [0,50 ; 0,55] donne 14/14 tandis que 0,45
			// laisse fuir deux questions sans réponse et 0,60 en perd une
			// vraie. C'est une calibration sur quatorze requêtes et un seul
			// modèle d'embedding: à recalibrer par corpus, la commande est
			// dans docs/evals/. Mettre null restaure le
			// comportement d'avant, qui répond toujours quelque chose.
			MinimumDenseScore: float64Ptr(0.52),
			// Détection d'une question sans réponse, active par défaut.
			// Les deux conditions doivent être réunies, et c'est ce qui la
			// rend sûre: mesuré sur le corpus d'évaluation, elle ferme
			// trois des quatre questions sans réponse sans coûter une
			// seule vraie réponse, et fait passer le rappel global de 74 %
			// à 78 %.
			//
			// Une variante plus agressive (marge sous 0,07) ferme la
			// quatrième mais perd une requête à terme rare. Elle n'est pas
			// retenue: une négative qui passe rend du bruit avec un score
			// honnêtement bas, une vraie réponse perdue est une absence
			// silencieuse, et le second défaut est plus grave que le
			// premier.
			NoAnswerBestBelow:   float64Ptr(0.58),
			NoAnswerMarginBelow: float64Ptr(0.05),
		},
		Graph: Graph{
			// false par défaut, et ça reste vrai maintenant que la couche
			// graphe existe et que ses deux handlers sont enregistrés dès
			// que graph.enabled vaut true: l'activer suppose un extracteur
			// joignable, et surtout, un appel à un modèle de langage pour
			// chaque message ingéré. Un fichier de configuration qui omet
			// le bloc graph n'a jamais demandé ce coût-là, et ne doit donc
			// pas se le voir imposer par un changement de défaut. Le
			// fichier d'exemple documente la même valeur, mais c'est ce
			// défaut-ci qui s'applique vraiment tant que l'opérateur n'a
			// rien écrit.
			Enabled: false,
			// false, et c'est mesuré. Sur le corpus étendu, avec le
			// classement des faits par pertinence à la question déjà en
			// place, faire entrer les candidats du graphe dans la fusion
			// fait tomber le rappel global de 73 % à 69 %: il gagne deux
			// requêtes de la famille graphe, que le dense et le lexical
			// résolvent de toute façon, et perd trois questions sans
			// réponse et une temporelle.
			//
			// Les faits, eux, continuent d'aller dans le context_block, et
			// c'est là que le graphe sert: répondre à une question à deux
			// sauts demande de raisonner sur la chaîne, ce que le critère 9
			// interdit sur le chemin de recherche et que le modèle lecteur
			// fait très bien si on lui donne la matière.
			FuseCandidates: false, ContextMessages: 4, MaxHops: 2,
			SingleValuedRelations: []string{"a_pour_etat"},
		},
		Rerank: Rerank{
			// Désactivé: c'est le seul appel de modèle du chemin de
			// recherche, et le critère 9 interdit d'en dépendre. Les
			// valeurs ci-dessous ne servent qu'à ce qu'un rerank.enabled
			// posé seul dans un fichier de configuration fonctionne.
			Enabled: false,
			Pool:    10,
			Timeout: 5 * time.Second,
		},
		Access: Access{DefaultScope: "participants"},
		Jobs: Jobs{Workers: 2, RetryLimit: 5, PollInterval: time.Second,
			ReclaimAfter: 10 * time.Minute},
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv remplace les ${VAR} par leur valeur. Une variable absente est une
// erreur: un DSN vide échouerait bien plus loin avec un message obscur.
func expandEnv(raw []byte) ([]byte, error) {
	var missing []string
	out := envRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(envRef.FindSubmatch(m)[1])
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return nil
		}
		return []byte(v)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined environment variables: %v", missing)
	}
	return out, nil
}

// ValidScopes énumère les scopes de conversation acceptés. Exportée pour que
// internal/api la référence plutôt que de dupliquer la même liste: une seule
// liste, un seul endroit à faire évoluer.
var ValidScopes = map[string]bool{
	"private": true, "participants": true, "workspace": true, "explicit": true,
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnv(raw)
	if err != nil {
		return nil, err
	}
	cfg := defaults()
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if !ValidScopes[c.Access.DefaultScope] {
		return fmt.Errorf("access.default_scope: unknown scope %q", c.Access.DefaultScope)
	}
	switch c.Service.ConsistencyDefault {
	case "eventual", "searchable":
	default:
		return fmt.Errorf("service.consistency_default: must be eventual or searchable")
	}
	if c.Service.MaxRequestBytes <= 0 {
		return fmt.Errorf("service.max_request_bytes must be positive")
	}
	if c.Service.MaxCandidateLimit <= 0 {
		return fmt.Errorf("service.max_candidate_limit must be positive")
	}
	if c.Service.MaxResultLimit <= 0 {
		return fmt.Errorf("service.max_result_limit must be positive")
	}
	if c.Service.MaxTokenBudget <= 0 {
		return fmt.Errorf("service.max_token_budget must be positive")
	}
	if c.Embedding.Dimensions <= 0 {
		return fmt.Errorf("embedding.dimensions must be positive")
	}
	switch c.Embedding.Provider {
	case EmbeddingProviderHTTP:
	case EmbeddingProviderGolem:
		if c.Embedding.ModelPath == "" {
			return fmt.Errorf("embedding.model_path is required when embedding.provider is golem")
		}
		if d := c.Embedding.Device; d != EmbeddingDeviceCPU && d != EmbeddingDeviceVulkan {
			return fmt.Errorf("embedding.device must be cpu or vulkan, got %q", d)
		}
	default:
		return fmt.Errorf("embedding.provider must be http or golem, got %q", c.Embedding.Provider)
	}
	if c.Indexing.CharsPerToken <= 0 {
		return fmt.Errorf("indexing.chars_per_token must be positive")
	}
	if min := 2 * JobHandlerTimeout; c.Jobs.ReclaimAfter < min {
		return fmt.Errorf(
			"jobs.reclaim_after (%s) must be at least twice the handler timeout (%s), "+
				"or a slow-but-alive handler gets reclaimed while still working: "+
				"attempts climbs on a healthy job, its work duplicates, and it is "+
				"eventually dead-lettered without ever having failed",
			c.Jobs.ReclaimAfter, min)
	}

	// graph.enabled sans extraction.base_url poserait des jobs
	// graph_extract qu'aucun appel réseau ne pourrait jamais honorer: ils
	// échoueraient à chaque tentative et finiraient en lettre morte, sans
	// jamais rien extraire. Un échec de démarrage, ici, vaut mieux qu'une
	// file qui se remplit de lettres mortes en silence.
	if c.Rerank.Enabled {
		if strings.TrimSpace(c.Rerank.BaseURL) == "" {
			return fmt.Errorf("rerank.enabled requires rerank.base_url")
		}
		if strings.TrimSpace(c.Rerank.Model) == "" {
			return fmt.Errorf("rerank.enabled requires rerank.model")
		}
		// Un pool qui ne dépasse pas final_top_k ne réordonne rien: il
		// soumet au modèle exactement ce qui allait être rendu de toute
		// façon, en payant l'appel pour rien.
		if c.Rerank.Pool <= c.Retrieval.FinalTopK {
			return fmt.Errorf("rerank.pool (%d) must exceed retrieval.final_top_k (%d)",
				c.Rerank.Pool, c.Retrieval.FinalTopK)
		}
		if c.Rerank.Timeout <= 0 {
			c.Rerank.Timeout = 5 * time.Second
		}
	}
	if c.Graph.Enabled && strings.TrimSpace(c.Extraction.BaseURL) == "" {
		return fmt.Errorf("graph.enabled requires extraction.base_url")
	}
	// max_hops à zéro ou moins ne veut rien dire pour une traversée: on
	// remonte à 1 plutôt que refuser, puisque 0 n'est écrit que par
	// omission, jamais par une intention délibérée de couper le graphe
	// (c'est graph.enabled qui porte cette intention-là).
	if c.Graph.MaxHops <= 0 {
		c.Graph.MaxHops = 1
	}
	// Au-delà de 3, une CTE récursive sur un graphe dense explose en temps
	// de requête, et la section 8.2 de la spec ne demande jamais plus de
	// deux sauts: la borne haute est un refus au démarrage, pas un
	// plafonnement silencieux qui masquerait une valeur absurde dans un
	// fichier de configuration.
	if c.Graph.MaxHops > 3 {
		return fmt.Errorf("graph.max_hops must be at most 3, got %d", c.Graph.MaxHops)
	}
	if c.Graph.ContextMessages < 0 {
		return fmt.Errorf("graph.context_messages must not be negative")
	}
	return nil
}

// float64Ptr rend un pointeur sur une valeur littérale. Les seuils optionnels
// de la configuration sont des *float64 pour distinguer « non renseigné » de
// « zéro », et un littéral n'est pas adressable en Go.
func float64Ptr(v float64) *float64 { return &v }
