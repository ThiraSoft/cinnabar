package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Les quatre bancs publiés par MemPalace ramenés à une même forme: des
// workspaces qui contiennent des conversations et des questions. Chaque
// chargeur reproduit le découpage du script Python correspondant, pour que
// les deux systèmes voient exactement les mêmes données et que la métrique
// calculée ici soit la leur.

type msgSpec struct {
	Author  string
	Role    string
	Content string
	At      time.Time
	// Unit est l'identifiant de l'unité notée par le banc: dia_id LoCoMo,
	// index de tour MemBench. Vide quand le banc note autre chose.
	Unit    string
	Session string
	// Turn porte, pour MemBench, les deux identifiants contre lesquels le
	// script d'origine compare la cible: sid (ou mid) et index global.
	TurnSID    int
	TurnGlobal int
}

type convSpec struct {
	ID           string
	Participants []string
	Msgs         []msgSpec
}

type questionSpec struct {
	ID        string
	Text      string
	Requester string
	Category  string

	EvidenceDia      []string // LoCoMo
	EvidenceSessions []string // LoCoMo, LongMemEval
	EvidenceTexts    []string // ConvoMem
	Targets          []int    // MemBench
}

type wsSpec struct {
	ID    string
	Convs []convSpec
	Qs    []questionSpec
}

// --- LoCoMo -----------------------------------------------------------------

type locomoTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

type locomoQA struct {
	Question string          `json:"question"`
	Evidence json.RawMessage `json:"evidence"`
	Category int             `json:"category"`
}

type locomoSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []locomoQA                 `json:"qa"`
}

var locomoDiaSession = regexp.MustCompile(`D(\d+):`)

func loadLoCoMo(path, prefix string) ([]wsSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data []locomoSample
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	var out []wsSpec
	for _, s := range data {
		ws := wsSpec{ID: prefix + s.SampleID}
		var a, b string
		_ = json.Unmarshal(s.Conversation["speaker_a"], &a)
		_ = json.Unmarshal(s.Conversation["speaker_b"], &b)
		parts := []string{userKey(a), userKey(b)}
		// Même parcours que load_conversation_sessions: session_1, 2... tant
		// que la clé existe.
		for n := 1; ; n++ {
			key := fmt.Sprintf("session_%d", n)
			rawTurns, ok := s.Conversation[key]
			if !ok {
				break
			}
			var turns []locomoTurn
			if err := json.Unmarshal(rawTurns, &turns); err != nil {
				return nil, fmt.Errorf("%s %s: %w", s.SampleID, key, err)
			}
			var dateStr string
			_ = json.Unmarshal(s.Conversation[key+"_date_time"], &dateStr)
			date, err := time.Parse("3:04 pm on 2 January, 2006", strings.ToLower(dateStr))
			if err != nil {
				date, err = time.Parse("3:04 pm on 2 january, 2006", strings.ToLower(dateStr))
				if err != nil {
					return nil, fmt.Errorf("%s %s: date %q: %w", s.SampleID, key, dateStr, err)
				}
			}
			c := convSpec{ID: fmt.Sprintf("%s_s%d", ws.ID, n), Participants: parts}
			for i, t := range turns {
				c.Msgs = append(c.Msgs, msgSpec{
					Author: userKey(t.Speaker), Role: "user", Content: t.Text,
					At: date.Add(time.Duration(i) * time.Minute), Unit: t.DiaID,
					Session: key,
				})
			}
			ws.Convs = append(ws.Convs, c)
		}
		for i, q := range s.QA {
			// Le script garde toutes les questions, catégorie 5 comprise, et
			// ne découpe pas les preuves multiples: on fait pareil.
			var ev []string
			if err := json.Unmarshal(q.Evidence, &ev); err != nil {
				ev = nil
			}
			sess := map[string]bool{}
			for _, e := range ev {
				if m := locomoDiaSession.FindStringSubmatch(e); m != nil {
					sess["session_"+m[1]] = true
				}
			}
			out := questionSpec{
				ID: fmt.Sprintf("%s#%d", s.SampleID, i), Text: q.Question,
				Requester: userKey(a), Category: strconv.Itoa(q.Category),
				EvidenceDia: dedup(ev), EvidenceSessions: keys(sess),
			}
			ws.Qs = append(ws.Qs, out)
		}
		out = append(out, ws)
	}
	return out, nil
}

// --- LongMemEval ------------------------------------------------------------

type lmeTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type lmeEntry struct {
	QuestionID       string      `json:"question_id"`
	QuestionType     string      `json:"question_type"`
	Question         string      `json:"question"`
	AnswerSessionIDs []string    `json:"answer_session_ids"`
	HaystackDates    []string    `json:"haystack_dates"`
	HaystackIDs      []string    `json:"haystack_session_ids"`
	HaystackSessions [][]lmeTurn `json:"haystack_sessions"`
}

func loadLongMemEval(path, prefix string, perType int) ([]wsSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data []lmeEntry
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	taken := map[string]int{}
	var out []wsSpec
	for qi, e := range data {
		if perType > 0 {
			if taken[e.QuestionType] >= perType {
				continue
			}
			taken[e.QuestionType]++
		}
		ws := wsSpec{ID: fmt.Sprintf("%sq%03d", prefix, qi)}
		parts := []string{"user:user", "agent:assistant"}
		for si, sess := range e.HaystackSessions {
			date, err := time.Parse("2006/01/02 (Mon) 15:04", e.HaystackDates[si])
			if err != nil {
				return nil, fmt.Errorf("%s: date %q: %w", e.QuestionID, e.HaystackDates[si], err)
			}
			c := convSpec{ID: fmt.Sprintf("%s_s%03d", ws.ID, si), Participants: parts}
			for ti, t := range sess {
				if strings.TrimSpace(t.Content) == "" {
					continue
				}
				author, role := "user:user", "user"
				if t.Role == "assistant" {
					author, role = "agent:assistant", "assistant"
				}
				c.Msgs = append(c.Msgs, msgSpec{
					Author: author, Role: role, Content: t.Content,
					At: date.Add(time.Duration(ti) * time.Minute), Session: e.HaystackIDs[si],
				})
			}
			if len(c.Msgs) > 0 {
				ws.Convs = append(ws.Convs, c)
			}
		}
		ws.Qs = []questionSpec{{
			ID: e.QuestionID, Text: e.Question, Requester: "user:user",
			Category: e.QuestionType, EvidenceSessions: e.AnswerSessionIDs,
		}}
		out = append(out, ws)
	}
	return out, nil
}

// --- ConvoMem ---------------------------------------------------------------

// Ordre et nom des catégories de convomem_bench.py. Les fichiers sont ceux
// que ce script a déjà téléchargés dans son répertoire de cache: on relit sa
// liste de fichiers et ses copies, pour tomber sur les mêmes items.
var convomemCategories = []string{
	"user_evidence", "assistant_facts_evidence", "changing_evidence",
	"abstention_evidence", "preference_evidence", "implicit_connection_evidence",
}

type convomemItem struct {
	Question      string `json:"question"`
	Conversations []struct {
		Messages []struct {
			Speaker string `json:"speaker"`
			Text    string `json:"text"`
		} `json:"messages"`
	} `json:"conversations"`
	MessageEvidences []struct {
		Text string `json:"text"`
	} `json:"message_evidences"`
}

func loadConvoMem(cacheDir, prefix string, limit int) ([]wsSpec, error) {
	base := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	var out []wsSpec
	for _, cat := range convomemCategories {
		var files []string
		raw, err := os.ReadFile(filepath.Join(cacheDir, cat+"_filelist.json"))
		if err != nil {
			continue // le script saute aussi une catégorie sans fichiers
		}
		if err := json.Unmarshal(raw, &files); err != nil {
			return nil, err
		}
		var items []convomemItem
		for _, f := range files {
			if len(items) >= limit {
				break
			}
			p := filepath.Join(cacheDir, cat, strings.ReplaceAll(f, "/", "_"))
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var doc struct {
				EvidenceItems []convomemItem `json:"evidence_items"`
			}
			if err := json.Unmarshal(b, &doc); err != nil {
				continue
			}
			items = append(items, doc.EvidenceItems...)
		}
		if len(items) > limit {
			items = items[:limit]
		}
		for i, it := range items {
			ws := wsSpec{ID: fmt.Sprintf("%s%s_%03d", prefix, shortCat(cat), i)}
			parts := []string{"user:user", "agent:assistant"}
			for ci, c := range it.Conversations {
				conv := convSpec{ID: fmt.Sprintf("%s_c%02d", ws.ID, ci), Participants: parts}
				day := base.AddDate(0, 0, ci)
				for mi, m := range c.Messages {
					if strings.TrimSpace(m.Text) == "" {
						continue
					}
					author, role := "user:user", "user"
					if strings.EqualFold(m.Speaker, "assistant") {
						author, role = "agent:assistant", "assistant"
					}
					conv.Msgs = append(conv.Msgs, msgSpec{
						Author: author, Role: role, Content: m.Text,
						At: day.Add(time.Duration(mi) * time.Minute),
					})
				}
				if len(conv.Msgs) > 0 {
					ws.Convs = append(ws.Convs, conv)
				}
			}
			var ev []string
			for _, e := range it.MessageEvidences {
				ev = append(ev, strings.ToLower(strings.TrimSpace(e.Text)))
			}
			ws.Qs = []questionSpec{{
				ID: ws.ID, Text: it.Question, Requester: "user:user",
				Category: cat, EvidenceTexts: dedup(ev),
			}}
			out = append(out, ws)
		}
	}
	return out, nil
}

func shortCat(c string) string { return strings.TrimSuffix(c, "_evidence") }

// --- MemBench ---------------------------------------------------------------

var membenchCategories = []struct{ name, file string }{
	{"simple", "simple.json"}, {"highlevel", "highlevel.json"},
	{"knowledge_update", "knowledge_update.json"}, {"comparative", "comparative.json"},
	{"conditional", "conditional.json"}, {"noisy", "noisy.json"},
	{"aggregative", "aggregative.json"}, {"highlevel_rec", "highlevel_rec.json"},
	{"lowlevel_rec", "lowlevel_rec.json"}, {"RecMultiSession", "RecMultiSession.json"},
	{"post_processing", "post_processing.json"},
}

var membenchTime = regexp.MustCompile(`(\d{4}-\d{2}-\d{2} \d{2}:\d{2})`)

// loadMemBench suit load_membench avec topic="movie": un fichier est un
// objet dont les clés sont des topics, on garde movie, roles et events, dans
// l'ordre du fichier. Go ne garde pas l'ordre des clés d'une map, d'où le
// décodage en json.Decoder.
func loadMemBench(dir, prefix string, perCat int) ([]wsSpec, error) {
	base := time.Date(2024, 10, 1, 8, 0, 0, 0, time.UTC)
	var out []wsSpec
	for _, c := range membenchCategories {
		f, err := os.Open(filepath.Join(dir, c.file))
		if err != nil {
			continue
		}
		topics, err := orderedTopics(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.file, err)
		}
		n := 0
		for _, t := range topics {
			if t.name != "movie" && t.name != "roles" && t.name != "events" {
				continue
			}
			for ii, item := range t.items {
				if perCat > 0 && n >= perCat {
					break
				}
				var it struct {
					MessageList json.RawMessage `json:"message_list"`
					QA          struct {
						Question string          `json:"question"`
						Target   json.RawMessage `json:"target_step_id"`
					} `json:"QA"`
				}
				if err := json.Unmarshal(item, &it); err != nil {
					return nil, err
				}
				sessions, err := membenchSessions(it.MessageList)
				if err != nil || len(sessions) == 0 || it.QA.Question == "" {
					continue
				}
				n++
				ws := wsSpec{ID: fmt.Sprintf("%s%s_%s_%03d", prefix, c.name, t.name, ii)}
				parts := []string{"user:user", "agent:assistant"}
				global := 0
				clock := base
				for si, sess := range sessions {
					conv := convSpec{ID: fmt.Sprintf("%s_s%02d", ws.ID, si), Participants: parts}
					for _, turn := range sess {
						sid := global
						if v, ok := turn["sid"].(float64); ok {
							sid = int(v)
						} else if v, ok := turn["mid"].(float64); ok {
							sid = int(v)
						}
						user := str(turn["user"], turn["user_message"])
						asst := str(turn["assistant"], turn["assistant_message"])
						at := clock.Add(time.Minute)
						if m := membenchTime.FindString(str(turn["time"], nil)); m != "" {
							if p, err := time.Parse("2006-01-02 15:04", m); err == nil && p.After(clock) {
								at = p
							}
						}
						clock = at.Add(30 * time.Second)
						unit := strconv.Itoa(global)
						if strings.TrimSpace(user) != "" {
							conv.Msgs = append(conv.Msgs, msgSpec{Author: "user:user", Role: "user",
								Content: user, At: at, Unit: unit, TurnSID: sid, TurnGlobal: global})
						}
						if strings.TrimSpace(asst) != "" {
							conv.Msgs = append(conv.Msgs, msgSpec{Author: "agent:assistant", Role: "assistant",
								Content: asst, At: at.Add(30 * time.Second), Unit: unit, TurnSID: sid, TurnGlobal: global})
						}
						global++
					}
					if len(conv.Msgs) > 0 {
						ws.Convs = append(ws.Convs, conv)
					}
				}
				var steps [][]json.RawMessage
				_ = json.Unmarshal(it.QA.Target, &steps)
				var targets []int
				for _, s := range steps {
					if len(s) >= 1 {
						var v float64
						if json.Unmarshal(s[0], &v) == nil {
							targets = append(targets, int(v))
						}
					}
				}
				ws.Qs = []questionSpec{{ID: ws.ID, Text: it.QA.Question, Requester: "user:user",
					Category: c.name, Targets: targets}}
				out = append(out, ws)
			}
		}
	}
	return out, nil
}

type topicItems struct {
	name  string
	items []json.RawMessage
}

func orderedTopics(f *os.File) ([]topicItems, error) {
	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var out []topicItems
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := dec.Decode(&items); err != nil {
			return nil, err
		}
		out = append(out, topicItems{name: tok.(string), items: items})
	}
	return out, nil
}

// membenchSessions normalise message_list: une liste plate de tours devient
// une session unique, comme dans index_turns.
func membenchSessions(raw json.RawMessage) ([][]map[string]any, error) {
	var flat []map[string]any
	if err := json.Unmarshal(raw, &flat); err == nil {
		return [][]map[string]any{flat}, nil
	}
	var nested []json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, err
	}
	var out [][]map[string]any
	for _, s := range nested {
		var turns []map[string]any
		if err := json.Unmarshal(s, &turns); err != nil {
			continue
		}
		out = append(out, turns)
	}
	return out, nil
}

func str(a, b any) string {
	if s, ok := a.(string); ok && s != "" {
		return s
	}
	if s, ok := b.(string); ok {
		return s
	}
	return ""
}

func userKey(name string) string {
	return "user:" + strings.ToLower(strings.ReplaceAll(name, " ", "_"))
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
