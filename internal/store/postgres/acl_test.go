package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// pinnedQueries énumère les requêtes de recherche du paquet, celles qui
// rendent des lignes à un demandeur et doivent donc porter la règle
// d'accès. TestEveryGuardedQueryIsClassified garantit qu'aucune requête du
// paquet touchant une table gardée ne manque à cette liste.
// guardedTablePatterns nomme les chemins par lesquels une requête atteint une
// table dont la lecture est soumise à la règle d'accès. Déclarée une seule
// fois: les deux garde-fous ci-dessous doivent voir exactement le même
// ensemble, et deux listes tenues côte à côte finissent par diverger sans que
// rien ne le dise.
//
// Les motifs sont en majuscules et comparés sur du SQL mis en majuscules: une
// requête écrite en minuscules échappait sinon aux deux.
func guardedTablePatterns() []string {
	return []string{
		"FROM MEMORY_UNITS",
		"FROM MESSAGES",
		"JOIN MESSAGES",
		"JOIN MEMORY_UNITS",
		"JOIN GRAPH_RELATION_SOURCES",
	}
}

func pinnedQueries() map[string]string {
	return map[string]string{
		"denseSQL":   denseSQL,
		"lexicalSQL": lexicalSQL,
		"graphSQL":   graphSQL,
	}
}

// maintenanceQueries énumère les requêtes qui touchent une table gardée
// sans être soumises à la règle d'accès, avec la raison. Ce ne sont pas des
// requêtes de recherche: elles ne rendent rien à un demandeur, elles
// agissent pour le compte du service lui-même, sur des lignes qu'il a déjà
// le droit de toucher parce que l'autorisation a eu lieu en amont.
//
// La liste est explicite exprès. Une requête de maintenance a le droit de
// contourner la règle, mais pas en silence: elle doit être nommée ici, avec
// la raison, par quelqu'un qui a réfléchi à la question.
func maintenanceQueries() map[string]string {
	return map[string]string{
		"touchedByMessageSQL": "réévaluation du graphe: liste les couples que la " +
			"disparition d'un message déplace, sans rien rendre à un demandeur",
		"invalidateSQL": "réévaluation du graphe: invalide les relations dont " +
			"toutes les sources ont disparu, décision du service et non lecture " +
			"pour le compte de quelqu'un",
		"enqueueConversationReevalSQL": "suppression d'une conversation: met en " +
			"file un graph_reeval par message qui source une relation, dans la " +
			"transaction de la suppression, sans rien rendre à un demandeur; " +
			"l'autorisation a eu lieu en amont dans handleDeleteConversation",
	}
}

// accessFragments énumère les fragments de SQL qui portent un morceau de la
// règle d'accès et sont composés dans une requête épinglée, sans être des
// requêtes autonomes. Ils touchent une table gardée, donc les deux garde-fous
// les voient; mais leur exiger un readable ou une jointure de garde n'aurait
// pas de sens, puisque c'est la requête qui les compose qui les porte.
//
// La contrepartie est vérifiée plus bas: un fragment nommé ici doit vraiment
// être composé dans au moins une requête épinglée. Sans ça, cette liste
// serait une échappatoire ouverte à toute constante qu'on y inscrirait.
func accessFragments() map[string]string {
	return map[string]string{
		"ExplicitACLSourceWindowPredicate": "borne un fait du graphe à la " +
			"fenêtre de l'unité octroyée quand seule une ligne memory_unit_acl " +
			"ouvre l'accès, composé dans graphSQL",
	}
}

// TestEverySearchQueryUsesReadableCTE est le garde-fou du critère 4 pour tout
// le paquet: toute requête SQL qui lit memory_units ou messages, qu'elle en
// parte (la recherche lexicale part de messages, pas de memory_units, voir
// search_lexical.go) ou qu'elle les atteigne par jointure (la recherche par
// graphe part de la CTE facts et remonte aux messages par
// graph_relation_sources, voir search_graph.go), doit préfixer readable
// (ReadableCTE), porter la jointure de garde de conversation
// (ConversationGuardJoin ou son pendant
// ConversationGuardJoinOnMessages, toutes deux écrites "JOIN conversations
// c") et combiner les deux branches d'accès (readable OR EXISTS sur
// memory_unit_acl). Une simple recherche de "readable" ne suffit pas: ce mot
// apparaît aussi dans le LEFT JOIN readable r, donc un test qui ne cherchait
// que lui restait vert même si tout le bloc AND (...) du prédicat d'accès
// disparaissait, ce qui aurait rendu toutes les lignes correspondantes du
// workspace sans plus aucun contrôle. C'est un test crude, énuméré à la main
// plutôt que réfléchi automatiquement sur le paquet, mais il échoue
// bruyamment le jour où une future stratégie (graphe) oublie l'un de ces
// mécanismes: une règle qui doit être retenue est une règle qui sera
// oubliée.
//
// La liste des requêtes examinées était elle-même tenue à la main, ce qui
// laissait exactement ce trou: la tâche 8 a livré graphSQL sans l'y inscrire
// et rien ne l'a signalé. TestEveryGuardedQueryIsClassified, plus bas, ferme
// ce trou en lisant les sources du paquet: aucune constante touchant une
// table gardée ne peut plus échapper aux deux classifications.
func TestEverySearchQueryUsesReadableCTE(t *testing.T) {
	queries := pinnedQueries()
	// Les motifs de déclenchement, élargis pour la stratégie graphe. Le SQL
	// du graphe part de FROM facts (une CTE) et n'atteint ni messages ni
	// memory_units autrement que par jointure. Il se trouve qu'il déclenche
	// quand même "FROM memory_units", mais uniquement parce que son
	// LEFT JOIN LATERAL de résolution d'unité contient ce texte: un
	// déclenchement de circonstance, qui disparaîtrait le jour où ce LATERAL
	// serait restructuré ou retiré, alors que la dérivation d'accès resterait
	// tout aussi obligatoire. Les deux motifs de jointure nomment donc les
	// chemins qui portent vraiment la règle: "JOIN messages", par lequel
	// toute stratégie rejoint la table gardée, et
	// "JOIN graph_relation_sources", par lequel une relation remonte à ses
	// messages sources. Une requête qui lit l'une de ces tables doit prouver
	// qu'elle passe bien par la règle d'accès des messages.
	guardedTablePatterns := guardedTablePatterns()
	for name, sql := range queries {
		selectsGuardedTable := false
		// Blancs repliés avant la comparaison, pour la raison écrite dans
		// normalizeSQL: un saut de ligne après FROM retirait sinon la requête
		// des deux garde-fous.
		folded := normalizeSQL(sql)
		upper := guardedTableBody(sql)
		for _, pattern := range guardedTablePatterns {
			if strings.Contains(upper, pattern) {
				selectsGuardedTable = true
				break
			}
		}
		if !selectsGuardedTable {
			continue
		}
		if !strings.Contains(folded, "readable") {
			t.Errorf("%s ne préfixe pas readable (ReadableCTE)", name)
		}
		if !strings.Contains(folded, "JOIN conversations c") {
			t.Errorf("%s n'a pas la jointure de garde de conversation (JOIN conversations c)", name)
		}
		// "OR EXISTS (" est spécifique au bloc AND (readable OR EXISTS ...)
		// qui combine les deux branches d'accès: contrairement à
		// "r.conversation_id IS NOT NULL" pris seul, cette phrase ne
		// réapparaît pas ailleurs dans la requête (accessReasonExpr, par
		// exemple, contient aussi "r.conversation_id IS NOT NULL" dans son
		// CASE d'affichage, mais jamais accolé à un "OR EXISTS").
		if !strings.Contains(folded, "OR EXISTS (") {
			t.Errorf("%s n'a pas le prédicat d'accès combinant readable et une ACL explicite (OR EXISTS)", name)
		}
	}
}

// TestValidateRequesterRejectsEmptyFields couvre ValidateRequester
// directement: c'est la fonction que SearchDense et SearchLexical doivent
// toutes deux appeler avant d'interroger la base, donc ses deux messages
// d'erreur doivent être distincts pour rester diagnostiquables séparément.
func TestValidateRequesterRejectsEmptyFields(t *testing.T) {
	if err := ValidateRequester("ws1", "user:paul"); err != nil {
		t.Fatalf("entrées valides: err = %v", err)
	}

	errEmptyWorkspace := ValidateRequester("", "user:paul")
	if errEmptyWorkspace == nil {
		t.Fatal("workspace_id vide doit être rejeté")
	}

	errEmptyRequester := ValidateRequester("ws1", "")
	if errEmptyRequester == nil {
		t.Fatal("requester_key vide doit être rejeté")
	}

	if errEmptyWorkspace.Error() == errEmptyRequester.Error() {
		t.Errorf("les deux rejets doivent porter des messages distincts, tous deux = %q",
			errEmptyWorkspace.Error())
	}
}

// TestEveryGuardedQueryIsClassified ferme le vrai trou du garde-fou ci-dessus.
//
// TestEverySearchQueryUsesReadableCTE vérifie les requêtes qu'on lui donne, et
// son propre commentaire l'admettait: la liste est tenue à la main. Une
// quatrième stratégie qui oublie d'inscrire sa requête n'échoue donc pas, elle
// n'est simplement jamais examinée. C'est exactement la forme de garantie que
// personne n'applique dont ce projet s'est déjà fait prendre plusieurs fois.
// La tâche 8 en a d'ailleurs été un cas: graphSQL ne figurait pas dans la
// liste, et rien ne l'a signalé.
//
// Ce test lit donc les sources du paquet et exige que toute constante de
// niveau paquet dont le texte touche une table gardée soit classée: soit
// requête de recherche, et alors elle passe par le garde-fou, soit requête de
// maintenance, et alors elle est nommée dans maintenanceQueries avec sa
// raison. Rien ne peut plus échapper aux deux.
//
// Limites connues et assumées, énumérées parce qu'une échappatoire non dite
// dans un garde-fou est pire qu'une échappatoire dite. Une revue les a
// trouvées en attaquant le test avec un fichier de leurres; celles qui se
// fermaient à peu de frais ont été fermées, les autres sont ici.
//
// Fermées depuis: le parcours accepte maintenant token.VAR en plus de
// token.CONST (une requête déclarée en var passait, alors même que le
// commentaire de denseSQL défend le const contre la réassignation), la
// comparaison se fait en majuscules repliées (une requête écrite en
// minuscules passait), et "JOIN MEMORY_UNITS" a rejoint les motifs (il
// manquait, alors que "JOIN MESSAGES" y était pour exactement la même
// raison, et qu'une stratégie partant de FROM conversations en joignant les
// unités n'aurait rien d'exotique).
//
// Fermées par la revue finale, qui a re-attaqué les deux garde-fous avec un
// fichier de leurres et en a trouvé quatre qui passaient, dont une seule était
// divulguée:
//
//   - Un saut de ligne entre FROM et le nom de la table. Écrire "FROM" puis
//     un retour à la ligne puis "\tmessages m" est une mise en forme
//     parfaitement ordinaire, que n'importe qui produit en reformatant une
//     requête longue, et elle retirait silencieusement la requête des deux
//     garde-fous. C'était la seule des quatre atteignable sans que personne
//     ait à mal faire, donc la plus grave. Fermée par normalizeSQL, qui
//     replie les blancs avant toute comparaison. Le commentaire de ce test ne
//     mentionnait « normaliser les blancs » que comme un coût à payer pour
//     fermer la jointure par virgule, jamais comme un trou à part entière.
//   - Un nom de table coupé entre deux littéraux. Fermée par literalStrings,
//     qui déblinde les littéraux avant de les concaténer au lieu de recopier
//     lit.Value avec ses guillemets.
//
// Fermées par la re-revue, qui a repris le fichier de leurres et trouvé trois
// formes de plus alors que cette liste se déclarait complète:
// FROM public.messages, FROM "messages" et JOIN public.graph_relation_sources.
// Aucune n'est exotique et aucune ne demande de mal faire: qualifier une table
// par son schéma ou en guillemeter le nom est une habitude, et un générateur
// de SQL produit les deux. Fermées par guardedTableBody, qui efface la
// qualification de schéma et les guillemets.
//
// Restent ouvertes, sciemment. Cette liste n'est PAS exhaustive et ne peut pas
// l'être: c'est l'état des trous connus après trois attaques, pas une preuve
// qu'il n'en reste aucun. La version précédente de ce commentaire se déclarait
// complète, et la re-revue a immédiatement trouvé trois formes qu'elle
// omettait; une liste qui se dit complète invite à ne plus chercher, ce qui
// est le contraire du service que ce garde-fou doit rendre.
//
//   - Une requête écrite en ligne dans le corps d'une fonction échappe au
//     parcours, qui ne voit que les déclarations de niveau paquet. La
//     convention du paquet est qu'une requête de recherche est une constante
//     de niveau paquet, et c'est cette convention que le test rend
//     contraignante.
//   - Une var de niveau paquet déclarée sans valeur et remplie dans un init()
//     échappe au parcours: vs.Values est vide, donc il n'y a aucun littéral à
//     lire. L'acceptation de token.VAR n'a fermé le cas var que pour les var
//     porteuses d'un littéral, contrairement à ce que ce commentaire laissait
//     entendre. La fermer demanderait de suivre les affectations dans les
//     corps de fonction, c'est-à-dire d'écrire une analyse de flot; le cas est
//     par ailleurs très improbable, aucune requête de ce paquet n'étant
//     construite ainsi.
//   - Une jointure écrite par virgule échappe aux motifs: "FROM conversations
//     c, messages m" ne contient ni "FROM MESSAGES" ni "JOIN MESSAGES", et le
//     repliement des blancs n'y change rien. Les fermer demanderait de
//     chercher les noms de tables plutôt que leurs préfixes FROM et JOIN, donc
//     d'accepter beaucoup plus de faux positifs. C'est un style SQL ancien
//     qu'on n'écrit plus par accident dans du code neuf, ce qui en fait le
//     moins probable des quatre.
//   - FROM"messages" sans espace avant le guillemet, qui est du SQL valide.
//     Le repliement des blancs ne peut rien y faire et le retrait des
//     guillemets recolle FROM au nom de la table. La fermer demanderait de
//     traiter les guillemets comme des séparateurs, ce qui ferait entrer les
//     identifiants guillemetés partout ailleurs dans la comparaison.
//   - Un commentaire SQL entre FROM et le nom de la table. La fermer
//     demanderait de retirer les commentaires du corps, donc de tokeniser le
//     SQL, ce qui est un autre métier que celui de ce test.
//   - Les motifs ne cherchent que des lectures. deactivateCoveringSQL
//     (UPDATE memory_units) et insertSourceSQL
//     (INSERT INTO graph_relation_sources) touchent une table gardée sans
//     être signalés. C'est un choix, pas un oubli: ce sont des écritures dont
//     l'autorisation a eu lieu en amont, et la règle d'accès de la section 8.3
//     porte sur ce qui est rendu à un demandeur. Le jour où ces motifs
//     s'élargissent aux écritures, ces deux requêtes iront dans
//     maintenanceQueries avec cette raison.
//
// Faux positif connu, et assumé dans ce sens-là: un motif écrit dans un
// commentaire SQL déclenche la classification. Il vaut mieux classer une
// requête pour rien que d'en laisser passer une.
func TestEveryGuardedQueryIsClassified(t *testing.T) {
	guardedTablePatterns := guardedTablePatterns()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("lecture des sources du paquet: %v", err)
	}

	pinned := pinnedQueries()
	maintenance := maintenanceQueries()
	fragments := accessFragments()
	found := 0

	// Un fragment nommé doit être réellement composé dans une requête
	// épinglée. Sinon accessFragments deviendrait une porte de sortie: il
	// suffirait d'y inscrire un nom pour retirer une requête des deux
	// classifications.
	for name := range fragments {
		composed := false
		for _, sql := range pinned {
			if strings.Contains(normalizeSQL(sql),
				normalizeSQL(fragmentSQL(t, name))) {
				composed = true
				break
			}
		}
		if !composed {
			t.Errorf("%s est nommé dans accessFragments mais n'est composé "+
				"dans aucune requête épinglée", name)
		}
	}

	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						body := guardedTableBody(literalStrings(vs.Values[i]))
						touches := false
						for _, pattern := range guardedTablePatterns {
							if strings.Contains(body, pattern) {
								touches = true
								break
							}
						}
						if !touches {
							continue
						}
						found++
						_, isPinned := pinned[name.Name]
						reason, isMaintenance := maintenance[name.Name]
						fragReason, isFragment := fragments[name.Name]
						switch {
						case isPinned && isMaintenance:
							t.Errorf("%s (%s) est classée à la fois requête de "+
								"recherche et requête de maintenance", name.Name, path)
						case isPinned || (isMaintenance && reason != "") ||
							(isFragment && fragReason != ""):
							// Classée, rien à dire.
						case isMaintenance:
							t.Errorf("%s (%s) est dans maintenanceQueries sans raison",
								name.Name, path)
						case isFragment:
							t.Errorf("%s (%s) est dans accessFragments sans raison",
								name.Name, path)
						default:
							t.Errorf("%s (%s) touche une table gardée et n'est classée "+
								"nulle part: l'inscrire dans pinnedQueries si elle rend "+
								"des lignes à un demandeur, dans maintenanceQueries avec "+
								"sa raison sinon, dans accessFragments si c'est un "+
								"fragment composé dans une requête épinglée",
								name.Name, path)
						}
					}
				}
			}
		}
	}

	// Si le parcours ne trouve plus rien, c'est le test lui-même qui est
	// cassé, pas le paquet qui est devenu propre.
	if found < len(pinned)+len(maintenance)+len(fragments) {
		t.Errorf("%d constantes touchant une table gardée trouvées, au moins %d "+
			"attendues: le parcours des sources ne fonctionne plus",
			found, len(pinned)+len(maintenance)+len(fragments))
	}
}

// literalStrings concatène toutes les chaînes littérales d'une expression.
// Une constante SQL de ce paquet s'écrit souvent comme une concaténation de
// littéraux et d'autres constantes (ReadableCTE, les jointures de garde); les
// motifs de table cherchés vivent dans les littéraux, et les constantes
// référencées sont examinées pour leur propre compte.
//
// Les littéraux sont déblindés avant d'être concaténés, et pas recopiés tels
// que le fichier les écrit. Sur lit.Value brut, les guillemets ou les
// apostrophes inverses de chaque littéral restaient entre les deux morceaux,
// si bien qu'un nom de table coupé entre deux littéraux
// (`"... FROM " + "messages m ..."`) ne contenait plus « FROM MESSAGES » et
// sortait des deux garde-fous. Le prix est un faux positif possible: deux
// littéraux qui se recollent en un motif que personne n'a écrit. C'est un
// faux positif qui force à classer, donc du bon côté.
func literalStrings(expr ast.Expr) string {
	var b strings.Builder
	ast.Inspect(expr, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil {
			b.WriteString(v)
		} else {
			b.WriteString(lit.Value)
		}
		return true
	})
	return b.String()
}

// normalizeSQL replie les blancs d'un corps SQL en une espace simple, sans
// toucher à la casse. C'est ce qui empêche une mise en forme ordinaire de
// retirer une requête des deux garde-fous: écrire
//
//	FROM
//	    messages m
//
// est parfaitement banal quand on reformate une requête longue, et les motifs
// cherchent « FROM MESSAGES » avec exactement une espace. La revue finale a
// mesuré que c'était la seule des échappatoires atteignable sans que personne
// ait à mal faire.
func normalizeSQL(sql string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(sql), " "), `"`, "")
}

// guardedTableBody rend le corps sous la forme où les motifs de table sont
// cherchés: blancs repliés, guillemets retirés, majuscules, et qualification de
// schéma effacée.
//
// Les trois formes que ça ferme, toutes trouvées par la re-revue alors que la
// divulgation de ce test se déclarait complète: FROM public.messages,
// FROM "messages" et JOIN public.graph_relation_sources. Aucune n'est exotique;
// un outil de génération ou une habitude de qualifier les tables en produit
// naturellement, et chacune retirait la requête des deux garde-fous.
//
// Le retrait des guillemets est sans risque ici: aucune constante SQL du
// paquet n'en contient, les littéraux SQL s'écrivant entre apostrophes.
func guardedTableBody(sql string) string {
	return strings.ReplaceAll(
		strings.ToUpper(normalizeSQL(sql)), "PUBLIC.", "")
}

// fragmentSQL rend la valeur du fragment nommé. Écrite comme un switch et non
// comme une table pour que le compilateur constate que la constante existe:
// une constante renommée casse la compilation du test au lieu de le laisser
// passer sur une chaîne devenue vide.
func fragmentSQL(t *testing.T, name string) string {
	t.Helper()
	switch name {
	case "ExplicitACLSourceWindowPredicate":
		return ExplicitACLSourceWindowPredicate
	default:
		t.Fatalf("fragment %q sans valeur: fragmentSQL et accessFragments "+
			"doivent rester en phase", name)
		return ""
	}
}
