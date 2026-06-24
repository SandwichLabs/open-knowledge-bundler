package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/store"
)

// Preset is a named graph-query that returns a small subgraph to visualize.
type Preset struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	HasQ        bool   `json:"has_q,omitempty"`  // accepts ?q= topic string
	HasN        bool   `json:"has_n,omitempty"`  // accepts ?n= top-N
}

// subgraph is the wire shape returned to the browser.
type subgraph struct {
	Nodes []sgNode `json:"nodes"`
	Edges []sgEdge `json:"edges"`
}
type sgNode struct {
	NodeID     string         `json:"node_id"`
	NodeType   string         `json:"node_type"`
	Properties map[string]any `json:"properties"`
}
type sgEdge struct {
	Source           string `json:"source_id"`
	Target           string `json:"target_id"`
	RelationshipType string `json:"relationship_type"`
}

// Catalogue of presets shown in the UI.
var presetCatalog = []Preset{
	{Name: "top-witnesses", Label: "Top witnesses + their locations",
		Description: "Top N persons by WITNESSED_BY edges, the incidents they witnessed, and the locations of those incidents.",
		HasN:        true},
	{Name: "fbi-network", Label: "FBI network (Hoover & associates)",
		Description: "John Edgar Hoover, every entity directly connected to him, plus shared incidents within that cluster."},
	{Name: "topic-cluster", Label: "Topic cluster (Maury Island, Roswell, …)",
		Description: "All incidents whose summary, date, or location mention the topic, plus their persons / locations / objects.",
		HasQ:        true},
	{Name: "co-witnesses", Label: "Co-witness pairs",
		Description: "Persons connected through incidents they jointly witnessed (top N most-witnessed incidents)."},
	{Name: "top-objects", Label: "Top objects & source docs",
		Description: "Top N objects by mentions plus the documents that name them."},
	{Name: "documents-mentions", Label: "Documents → entities",
		Description: "Sample of documents and the persons / locations / objects they mention (limited for legibility)."},
}

func handlePresetList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, presetCatalog)
}

func handlePreset(w http.ResponseWriter, r *http.Request, db *store.DB) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, 400, fmt.Errorf("missing ?name"))
		return
	}
	q := r.URL.Query().Get("q")
	n := 20
	if v := r.URL.Query().Get("n"); v != "" {
		if x := atoiClamp(v, 1, 100); x > 0 {
			n = x
		}
	}

	var (
		sg  subgraph
		err error
	)
	switch name {
	case "top-witnesses":
		sg, err = presetTopWitnesses(db, n)
	case "fbi-network":
		sg, err = presetFBINetwork(db)
	case "topic-cluster":
		topic := q
		if topic == "" {
			topic = "Maury Island"
		}
		sg, err = presetTopicCluster(db, topic)
	case "co-witnesses":
		sg, err = presetCoWitnesses(db, n)
	case "top-objects":
		sg, err = presetTopObjects(db, n)
	case "documents-mentions":
		sg, err = presetDocumentMentions(db, n)
	default:
		writeErr(w, 404, fmt.Errorf("unknown preset %q", name))
		return
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, sg)
}

// ---------- helpers --------------------------------------------------------

func atoiClamp(s string, lo, hi int) int {
	x := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		x = x*10 + int(c-'0')
		if x > 1<<20 {
			return 0
		}
	}
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func collectNodes(db *store.DB, ids map[string]struct{}) ([]sgNode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	quoted := make([]string, 0, len(ids))
	for id := range ids {
		quoted = append(quoted, "'"+escapeSQL(id)+"'")
	}
	sql := fmt.Sprintf(
		"SELECT node_id, node_type, properties::VARCHAR FROM Nodes_Base WHERE is_current AND node_id IN (%s)",
		strings.Join(quoted, ","),
	)
	rows, err := db.RawQuery(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sgNode
	for rows.Next() {
		var nid, ntype, props string
		if err := rows.Scan(&nid, &ntype, &props); err != nil {
			return nil, err
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(props), &p)
		out = append(out, sgNode{nid, ntype, p})
	}
	return out, rows.Err()
}

func collectEdges(db *store.DB, ids map[string]struct{}, rels []string) ([]sgEdge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	quoted := make([]string, 0, len(ids))
	for id := range ids {
		quoted = append(quoted, "'"+escapeSQL(id)+"'")
	}
	relsClause := ""
	if len(rels) > 0 {
		q := make([]string, 0, len(rels))
		for _, r := range rels {
			q = append(q, "'"+escapeSQL(r)+"'")
		}
		relsClause = " AND relationship_type IN (" + strings.Join(q, ",") + ")"
	}
	idIn := strings.Join(quoted, ",")
	sql := fmt.Sprintf(`
		SELECT source_id, target_id, relationship_type
		FROM Edges_Base
		WHERE is_current%s
		  AND source_id IN (%s)
		  AND target_id IN (%s)`,
		relsClause, idIn, idIn,
	)
	rows, err := db.RawQuery(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sgEdge
	for rows.Next() {
		var e sgEdge
		if err := rows.Scan(&e.Source, &e.Target, &e.RelationshipType); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- presets --------------------------------------------------------

// Top N persons by WITNESSED_BY incident count + a sample of those incidents + their locations.
// Caps to ~3 incidents per witness so the graph stays legible.
func presetTopWitnesses(db *store.DB, n int) (subgraph, error) {
	sql := fmt.Sprintf(`
		WITH top_persons AS (
		  SELECT e.target_id AS pid, COUNT(DISTINCT e.source_id) AS c
		  FROM Edges_Base e
		  JOIN Nodes_Base p ON e.target_id = p.node_id AND p.is_current AND p.node_type='Person'
		  JOIN Nodes_Base i ON e.source_id = i.node_id AND i.is_current AND i.node_type='Incident'
		  WHERE e.relationship_type='WITNESSED_BY' AND e.is_current
		  GROUP BY 1 ORDER BY c DESC LIMIT %d
		),
		ranked_incidents AS (
		  SELECT e.target_id AS pid, e.source_id AS iid,
		         row_number() OVER (PARTITION BY e.target_id ORDER BY e.source_id) AS rn
		  FROM Edges_Base e
		  JOIN top_persons t ON e.target_id = t.pid
		  WHERE e.relationship_type='WITNESSED_BY' AND e.is_current
		),
		incidents AS (SELECT DISTINCT iid FROM ranked_incidents WHERE rn <= 3),
		locations AS (
		  SELECT DISTINCT e.target_id AS lid FROM Edges_Base e
		  JOIN incidents i ON e.source_id = i.iid
		  WHERE e.relationship_type='OCCURRED_AT' AND e.is_current
		)
		SELECT pid AS id FROM top_persons
		UNION SELECT iid FROM incidents
		UNION SELECT lid FROM locations`, n)
	return runSubgraph(db, sql, []string{"WITNESSED_BY", "OCCURRED_AT"})
}

// Everything Hoover touches. Restricts to Person + Agency + Incident for legibility.
func presetFBINetwork(db *store.DB) (subgraph, error) {
	sql := `
		WITH hoover AS (SELECT 'person:john-edgar-hoover' AS id),
		neighbors AS (
		  SELECT e.target_id AS id FROM Edges_Base e
		  JOIN Nodes_Base n ON e.target_id = n.node_id AND n.is_current
		  WHERE e.is_current AND e.source_id = (SELECT id FROM hoover)
		    AND n.node_type IN ('Person','Agency','Incident')
		  UNION
		  SELECT e.source_id FROM Edges_Base e
		  JOIN Nodes_Base n ON e.source_id = n.node_id AND n.is_current
		  WHERE e.is_current AND e.target_id = (SELECT id FROM hoover)
		    AND n.node_type IN ('Person','Agency','Incident')
		)
		SELECT id FROM hoover UNION SELECT id FROM neighbors LIMIT 150`
	return runSubgraph(db, sql, nil)
}

// Incidents matching topic + their 1-hop persons / locations / objects.
func presetTopicCluster(db *store.DB, topic string) (subgraph, error) {
	t := escapeSQL(strings.ToLower(topic))
	sql := fmt.Sprintf(`
		WITH inc AS (
		  SELECT node_id AS id FROM Nodes_Base
		  WHERE is_current AND node_type='Incident'
		    AND (
		      LOWER(properties->>'summary')       LIKE '%%%[1]s%%'
		      OR LOWER(properties->>'location_text') LIKE '%%%[1]s%%'
		      OR LOWER(properties->>'date_text')     LIKE '%%%[1]s%%'
		    )
		  LIMIT 80
		),
		one_hop AS (
		  SELECT target_id AS id FROM Edges_Base e JOIN inc ON e.source_id = inc.id WHERE e.is_current
		  UNION
		  SELECT source_id AS id FROM Edges_Base e JOIN inc ON e.target_id = inc.id WHERE e.is_current
		)
		SELECT id FROM inc UNION SELECT id FROM one_hop`, t)
	return runSubgraph(db, sql, []string{"WITNESSED_BY", "OCCURRED_AT", "REPORTED_BY", "MENTIONS"})
}

// Person↔Person edges (synthetic) inferred from shared incidents.
func presetCoWitnesses(db *store.DB, n int) (subgraph, error) {
	sql := fmt.Sprintf(`
		WITH busy_incidents AS (
		  SELECT source_id AS iid, COUNT(*) AS witnesses
		  FROM Edges_Base
		  WHERE is_current AND relationship_type='WITNESSED_BY'
		  GROUP BY 1 HAVING witnesses >= 2 ORDER BY witnesses DESC LIMIT %d
		),
		nodes AS (
		  SELECT iid AS id FROM busy_incidents
		  UNION
		  SELECT e.target_id FROM Edges_Base e JOIN busy_incidents b ON e.source_id = b.iid
		  WHERE e.is_current AND e.relationship_type='WITNESSED_BY'
		)
		SELECT id FROM nodes`, n)
	return runSubgraph(db, sql, []string{"WITNESSED_BY"})
}

// Top objects by mentions + the documents that mention them.
func presetTopObjects(db *store.DB, n int) (subgraph, error) {
	sql := fmt.Sprintf(`
		WITH top_objs AS (
		  SELECT node_id AS id
		  FROM Nodes_Base
		  WHERE is_current AND node_type='Object'
		  ORDER BY TRY_CAST(properties->>'mentions' AS INTEGER) DESC NULLS LAST
		  LIMIT %d
		),
		docs AS (
		  SELECT DISTINCT e.source_id AS id FROM Edges_Base e JOIN top_objs t ON e.target_id = t.id
		  WHERE e.relationship_type='MENTIONS' AND e.is_current
		  LIMIT 60
		)
		SELECT id FROM top_objs UNION SELECT id FROM docs`, n)
	return runSubgraph(db, sql, []string{"MENTIONS"})
}

// A small sample of documents and what they mention.
func presetDocumentMentions(db *store.DB, n int) (subgraph, error) {
	sql := fmt.Sprintf(`
		WITH big_docs AS (
		  SELECT d.node_id AS id, COUNT(*) AS c
		  FROM Edges_Base e
		  JOIN Nodes_Base d ON e.source_id = d.node_id AND d.is_current AND d.node_type='Document'
		  WHERE e.is_current AND e.relationship_type='MENTIONS'
		  GROUP BY 1 ORDER BY c DESC LIMIT %d
		),
		mentioned AS (
		  SELECT e.target_id AS id
		  FROM Edges_Base e JOIN big_docs ON e.source_id = big_docs.id
		  WHERE e.is_current AND e.relationship_type='MENTIONS'
		  LIMIT 200
		)
		SELECT id FROM big_docs UNION SELECT id FROM mentioned`, n)
	return runSubgraph(db, sql, []string{"MENTIONS"})
}

// runSubgraph executes an id-producing SQL and assembles the {nodes, edges} response.
func runSubgraph(db *store.DB, idSQL string, rels []string) (subgraph, error) {
	rows, err := db.RawQuery(idSQL)
	if err != nil {
		return subgraph{}, err
	}
	defer rows.Close()
	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return subgraph{}, err
		}
		ids[id] = struct{}{}
	}
	rows.Close()

	nodes, err := collectNodes(db, ids)
	if err != nil {
		return subgraph{}, err
	}
	edges, err := collectEdges(db, ids, rels)
	if err != nil {
		return subgraph{}, err
	}
	return subgraph{Nodes: nodes, Edges: edges}, nil
}
