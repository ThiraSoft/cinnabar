package postgres

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ThiraSoft/cinnabar/internal/memory"
)

// Ce fichier épingle l'ordre de verrouillage de graph_relations par son
// mécanisme et non par son émergence.
//
// Les trois tests croisés qui existent par ailleurs prennent des verrous
// réels et comptent les 40P01. Deux d'entre eux mordent franchement, mais le
// troisième, celui du chemin d'invalidation, ne mord qu'à un calibrage
// temporel près: la re-revue l'a mesuré à zéro interblocage sous charge CPU,
// et à zéro à 20 ms comme à 160 ms de décalage. Il est sur une pente, pas sur
// un plateau, et un test de concurrence qui rassure sans protéger est plus
// dangereux qu'un test absent.
//
// Ce que ces tests-ci épinglent est déterministe et ne dépend d'aucun
// ordonnancement: l'ordre de verrouillage est décidé en Go par des fonctions
// de tri, donc ces fonctions se testent en unitaire, sans base; et le fait que
// les deux chemins d'écriture prennent leurs verrous dans cet ordre-là est une
// propriété du texte du code, donc elle s'épingle par inspection des sources.
// C'est exactement la technique que ce paquet emploie déjà pour son invariant
// le plus sensible, la règle d'accès (voir acl_test.go).

// entiteTest fabrique un identifiant d'entité dont le premier octet est
// contrôlé, pour que l'ordre attendu soit lisible dans le test plutôt que
// dépendant d'un hachage.
func entiteTest(premier byte) uuid.UUID {
	var id uuid.UUID
	id[0] = premier
	return id
}

func relationTest(premier byte) uuid.UUID {
	var id uuid.UUID
	id[0] = premier
	id[15] = 0xff
	return id
}

// TestOrdreDeVerrouillageDesRelations épingle l'ordre que sortedRelations
// impose: le couple (entité source, type de relation) d'abord, relation_id
// ensuite. C'est l'ordre de sortedChainPairs puis celui de chainSelectSQL, et
// c'est ce qui aligne Apply sur Reevaluate.
//
// Trier par relation_id seul ne suffit pas, et le test le prouve plutôt que de
// l'affirmer: l'ordre attendu ci-dessous n'est pas celui des relation_id, il
// est même exactement son inverse à l'intérieur du premier couple.
func TestOrdreDeVerrouillageDesRelations(t *testing.T) {
	a, b := entiteTest(0x10), entiteTest(0x20)
	rel := func(src uuid.UUID, rtype string, id byte) memory.GraphRelation {
		return memory.GraphRelation{
			RelationID: relationTest(id), SourceEntityID: src,
			RelationType: rtype, WorkspaceID: "ws1",
		}
	}

	// Entrée dans un ordre qui n'est ni celui des couples ni celui des
	// identifiants.
	in := []memory.GraphRelation{
		rel(b, "likes", 0x01),
		rel(a, "owns", 0x05),
		rel(a, "likes", 0x09),
		rel(a, "likes", 0x03),
		rel(b, "likes", 0x07),
	}
	// Attendu: entité a avant entité b (0x10 < 0x20); dans a, "likes" avant
	// "owns"; dans (a, likes), 0x03 avant 0x09.
	want := []byte{0x03, 0x09, 0x05, 0x01, 0x07}

	got := sortedRelations(in)
	if len(got) != len(want) {
		t.Fatalf("%d relations rendues, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].RelationID != relationTest(w) {
			t.Fatalf("position %d: relation_id premier octet = %#x, want %#x; "+
				"l'ordre doit être (entité source, type, relation_id)",
				i, got[i].RelationID[0], w)
		}
	}

	// Le slice de l'appelant n'est pas réordonné sous ses pieds: Apply reçoit
	// l'extraction d'un handler qui peut encore s'en servir.
	if in[0].RelationID != relationTest(0x01) {
		t.Error("sortedRelations a réordonné le slice de l'appelant")
	}
}

// TestOrdreDeVerrouillageDeLInvalidation épingle que sortedSourcedIDs rend
// exactement l'ordre de sortedRelations sur les mêmes lignes.
//
// Les deux fonctions ne peuvent pas partager de code, l'une triant des
// relations à écrire et l'autre des identifiants lus en base, et c'est
// précisément pour ça qu'elles doivent être comparées: deux tris tenus côte à
// côte finissent par diverger sans que rien ne le dise, et une divergence ici
// est un 40P01 en production, pas un test rouge.
func TestOrdreDeVerrouillageDeLInvalidation(t *testing.T) {
	a, b := entiteTest(0x10), entiteTest(0x20)
	type ligne struct {
		src   uuid.UUID
		rtype string
		id    byte
	}
	lignes := []ligne{
		{b, "likes", 0x01},
		{a, "owns", 0x05},
		{a, "likes", 0x09},
		{a, "likes", 0x03},
		{b, "likes", 0x07},
	}

	var rels []memory.GraphRelation
	var sourced []sourcedRelation
	for _, l := range lignes {
		rels = append(rels, memory.GraphRelation{
			RelationID: relationTest(l.id), SourceEntityID: l.src,
			RelationType: l.rtype, WorkspaceID: "ws1"})
		sourced = append(sourced, sourcedRelation{
			id:   relationTest(l.id),
			pair: chainPair{workspace: "ws1", source: l.src, rtype: l.rtype}})
	}

	viaApply := sortedRelations(rels)
	viaReevaluate := sortedSourcedIDs(sourced)
	if len(viaReevaluate) != len(viaApply) {
		t.Fatalf("%d identifiants, want %d", len(viaReevaluate), len(viaApply))
	}
	for i := range viaApply {
		if viaApply[i].RelationID != viaReevaluate[i] {
			t.Fatalf("position %d: Apply verrouille %v, Reevaluate %v; les deux "+
				"chemins doivent prendre les mêmes lignes dans le même ordre",
				i, viaApply[i].RelationID, viaReevaluate[i])
		}
	}

	// Et cet ordre est bien celui des couples et pas celui des identifiants,
	// sans quoi l'égalité ci-dessus serait vraie pour la mauvaise raison.
	if bytes.Compare(viaReevaluate[0][:], viaReevaluate[1][:]) >= 0 {
		t.Log("note: le premier couple est ici dans l'ordre des identifiants")
	}
	if viaReevaluate[3] != relationTest(0x01) {
		t.Errorf("position 3 = %v, want la relation 0x01 du second couple: "+
			"l'ordre n'est pas celui des couples", viaReevaluate[3])
	}
}

// TestOrdreDesCouplesEstLePrefixeDeCeluiDesRelations épingle que
// sortedChainPairs et sortedRelations s'accordent sur l'ordre des couples.
//
// Les deux chemins verrouillent en deux temps: les chaînes des couples
// touchés, puis les lignes. Si les deux étapes ne rangeaient pas les couples
// pareil, l'alignement obtenu par la première serait défait par la seconde.
func TestOrdreDesCouplesEstLePrefixeDeCeluiDesRelations(t *testing.T) {
	a, b, c := entiteTest(0x10), entiteTest(0x20), entiteTest(0x30)
	touched := map[chainPair]bool{
		{workspace: "ws1", source: c, rtype: "likes"}: true,
		{workspace: "ws1", source: a, rtype: "owns"}:  true,
		{workspace: "ws1", source: a, rtype: "likes"}: true,
		{workspace: "ws1", source: b, rtype: "likes"}: true,
	}
	var rels []memory.GraphRelation
	i := byte(1)
	for p := range touched {
		rels = append(rels, memory.GraphRelation{
			RelationID: relationTest(i), SourceEntityID: p.source,
			RelationType: p.rtype, WorkspaceID: p.workspace})
		i++
	}

	pairs := sortedChainPairs(touched)
	sorted := sortedRelations(rels)
	if len(pairs) != len(sorted) {
		t.Fatalf("%d couples pour %d relations: le montage doit avoir une "+
			"relation par couple", len(pairs), len(sorted))
	}
	for i := range pairs {
		if pairs[i].source != sorted[i].SourceEntityID ||
			pairs[i].rtype != sorted[i].RelationType {
			t.Fatalf("position %d: sortedChainPairs range (%#x, %s), "+
				"sortedRelations (%#x, %s); les deux étapes de verrouillage "+
				"doivent ranger les couples pareil",
				i, pairs[i].source[0], pairs[i].rtype,
				sorted[i].SourceEntityID[0], sorted[i].RelationType)
		}
	}
}

// TestChainSelectOrdonneParRelationId épingle par inspection de chaîne que la
// requête de chaîne ordonne par relation_id.
//
// observed_at ne donnait pas un ordre total (deux observations simultanées
// sortaient dans un ordre que rien ne fixe) et surtout ne pouvait pas
// coïncider avec celui de sortedRelations, qui ne dispose que d'identifiants
// au moment de l'upsert. Le remettre est une régression silencieuse: la suite
// reste verte, graph.Chain retriant par observed_at en Go, et seuls les
// interblocages reviennent.
func TestChainSelectOrdonneParRelationId(t *testing.T) {
	folded := strings.Join(strings.Fields(chainSelectSQL), " ")
	if !strings.Contains(folded, "ORDER BY relation_id FOR UPDATE") {
		t.Errorf("chainSelectSQL doit ordonner par relation_id juste avant "+
			"FOR UPDATE, pour que l'ordre de verrouillage soit celui de "+
			"sortedRelations:\n%s", folded)
	}
	if strings.Contains(folded, "ORDER BY observed_at") {
		t.Error("chainSelectSQL ordonne par observed_at: ce n'est pas un ordre " +
			"total, et il ne peut pas coïncider avec celui de l'upsert")
	}
	// Le tri est un ordre de verrouillage et pas un ordre de lecture: si la
	// logique de chaîne en dépendait, on ne pourrait pas le changer.
	if !strings.Contains(folded, "FOR UPDATE") {
		t.Error("chainSelectSQL sans FOR UPDATE ne verrouille plus rien")
	}
}

// TestLockRelationsOrdonneParLOrdreRecu épingle que le pré-verrouillage des
// lignes de l'invalidation suit l'ordre du tableau reçu, décidé en Go, et non
// un tri SQL.
//
// Trier côté SQL par (source_entity_id, relation_type, relation_id) donnerait
// presque le même ordre, mais relation_type est du texte: son ordre
// dépendrait de la collation de la base alors que sortedRelations et
// sortedChainPairs comparent des octets en Go. La divergence ne se verrait
// que sur deux types du même sujet, dans une base à collation non C, et se
// manifesterait par un 40P01 intermittent.
func TestLockRelationsOrdonneParLOrdreRecu(t *testing.T) {
	folded := strings.Join(strings.Fields(lockRelationsSQL), " ")
	if !strings.Contains(folded, "ORDER BY array_position($1, relation_id)") {
		t.Errorf("lockRelationsSQL doit ordonner par array_position sur le "+
			"tableau reçu, pour que l'ordre vienne de Go et pas d'une "+
			"collation:\n%s", folded)
	}
	if !strings.Contains(folded, "FOR UPDATE") {
		t.Error("lockRelationsSQL sans FOR UPDATE ne verrouille rien")
	}
	for _, colonne := range []string{"ORDER BY source_entity_id",
		"ORDER BY relation_type"} {
		if strings.Contains(folded, colonne) {
			t.Errorf("lockRelationsSQL trie côté SQL (%s): l'ordre doit être "+
				"celui décidé en Go", colonne)
		}
	}
}

// TestLesDeuxCheminsPreVerrouillentLesChaines épingle, par inspection des
// sources, que les deux chemins d'écriture prennent les verrous de chaîne
// avant de toucher les lignes, et dans l'ordre trié.
//
// C'est la propriété que les tests croisés mesurent et que ceux-ci
// garantissent. Apply prenait d'abord les lignes qu'elle réécrit puis le reste
// de leur couple: une extraction qui ne réaffirme qu'une observation sur deux
// verrouillait donc la seconde ligne du couple avant la première, dans l'ordre
// inverse de Reevaluate. Et Reevaluate laissait son UPDATE d'invalidation
// prendre ses verrous dans l'ordre de son plan.
//
// Le test lit les positions dans le fichier source, donc il vérifie un ordre
// d'écriture et pas seulement une présence: déplacer le pré-verrouillage après
// la boucle d'upsert le fait échouer.
func TestLesDeuxCheminsPreVerrouillentLesChaines(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return fi.Name() == "graph_relations.go"
	}, 0)
	if err != nil {
		t.Fatalf("lecture des sources: %v", err)
	}

	corps := map[string]*ast.FuncDecl{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil {
					continue
				}
				corps[fn.Name.Name] = fn
			}
		}
	}
	if len(corps) == 0 {
		t.Fatal("aucune méthode trouvée dans graph_relations.go: le parcours " +
			"des sources ne fonctionne plus")
	}

	// premiere rend la position de la première mention d'un identifiant dans
	// le corps, ou -1.
	premiere := func(fn *ast.FuncDecl, nom string) int {
		pos := -1
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && id.Name == nom && (pos < 0 || int(id.Pos()) < pos) {
				pos = int(id.Pos())
			}
			return true
		})
		return pos
	}

	for _, cas := range []struct {
		methode string
		// avant doit apparaître avant apres.
		avant, apres string
		pourquoi     string
	}{
		{"Apply", "lockChainTx", "upsertRelationSQL",
			"Apply doit prendre les verrous de chaîne avant ses upserts, sinon " +
				"elle verrouille une partie du couple avant le reste"},
		{"Apply", "sortedChainPairs", "lockChainTx",
			"les couples doivent être triés avant d'être verrouillés"},
		{"Apply", "sortedRelations", "upsertRelationSQL",
			"les relations doivent être triées avant la boucle d'upsert"},
		{"Reevaluate", "lockChainTx", "invalidateSQL",
			"Reevaluate doit prendre les verrous de chaîne avant l'UPDATE " +
				"d'invalidation"},
		{"Reevaluate", "lockRelationsSQL", "invalidateSQL",
			"Reevaluate doit pré-verrouiller les lignes dans un ordre choisi " +
				"avant de laisser l'UPDATE les prendre dans celui de son plan"},
		{"Reevaluate", "sortedSourcedIDs", "lockRelationsSQL",
			"les identifiants doivent être triés avant d'être verrouillés"},
	} {
		fn, ok := corps[cas.methode]
		if !ok {
			t.Errorf("méthode %s introuvable", cas.methode)
			continue
		}
		a, b := premiere(fn, cas.avant), premiere(fn, cas.apres)
		switch {
		case a < 0:
			t.Errorf("%s ne mentionne pas %s: %s", cas.methode, cas.avant, cas.pourquoi)
		case b < 0:
			t.Errorf("%s ne mentionne pas %s: le test ne peut plus vérifier "+
				"l'ordre", cas.methode, cas.apres)
		case a > b:
			t.Errorf("dans %s, %s apparaît après %s: %s",
				cas.methode, cas.avant, cas.apres, cas.pourquoi)
		}
	}
}
