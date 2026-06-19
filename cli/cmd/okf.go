package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/domain"
	"github.com/sandwich-labs/chicago-business-intelligence/cli/store"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

const okfVersion = "0.1"

var (
	okfOutDir     string
	okfMode       string
	okfMaxPerType int
	okfNodeTypes  string
	okfSkill      bool
	okfIncludeDB  bool
)

var okfCmd = &cobra.Command{
	Use:   "okf",
	Short: "Export the knowledge graph as an Open Knowledge Format (OKF) bundle",
	Long: `Compiles the knowledge graph into an OKF v0.1 bundle: a directory tree of
markdown files with YAML frontmatter, readable by humans and agents without any
tooling (see https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md).

Modes (--mode):
  full     One concept document per node, grouped into <NodeType>/ directories,
           with edges rendered as cross-links. Faithful but can be large.
  catalog  One concept per node type and per relationship type, derived from the
           domain config and graph statistics. Small, curated schema overview.
  both     Catalog plus the full per-node dump (default).

Use --max-per-type to cap how many per-node documents are written for each type
(0 = unlimited); excess nodes are dropped from the dump (links to them become
tolerated broken links, per the spec).

Use --skill to also emit a SKILL.md so the bundle doubles as a self-describing
agent skill, and --include-db to copy the DuckDB database and domain config into
the bundle. Combined (--skill --include-db), the result is a fully self-contained
skill: browsable OKF markdown for orientation plus a queryable database for
precise hybrid (vector + lexical + graph) retrieval.

Output (under <out>/):
  index.md            bundle root listing (carries okf_version frontmatter)
  log.md              initialization entry
  catalog/            per-type / per-relationship schema concepts (catalog, both)
  <NodeType>/         per-node concept documents + index.md (full, both)
  SKILL.md            agent usage guide (--skill)
  <db>.duckdb         copy of the knowledge graph database (--include-db)
  domain.yaml         copy of the domain config (--include-db)`,
	RunE: runOKF,
}

func runOKF(cmd *cobra.Command, args []string) error {
	switch okfMode {
	case "full", "catalog", "both":
	default:
		return fmt.Errorf("invalid --mode %q (want full, catalog, or both)", okfMode)
	}

	dbPath := viper.GetString("database_path")
	if dbPath == "" {
		dbPath = "domain.duckdb"
	}
	domainName := viper.GetString("domain_name")

	nodeDefs, edgeDefs, err := readDefinitions(viper.ConfigFileUsed())
	if err != nil {
		return fmt.Errorf("reading definitions: %w", err)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()
	if err := db.LoadExtensions(); err != nil {
		return fmt.Errorf("loading extensions: %w", err)
	}

	nodes, err := readOKFNodes(db)
	if err != nil {
		return fmt.Errorf("reading nodes: %w", err)
	}
	edges, err := readEdges(db)
	if err != nil {
		return fmt.Errorf("reading edges: %w", err)
	}

	log.Printf("Domain:    %s", domainName)
	log.Printf("Database:  %s", dbPath)
	log.Printf("Mode:      %s", okfMode)
	log.Printf("Nodes:     %d", len(nodes))
	log.Printf("Edges:     %d", len(edges))

	b := &okfBundle{
		outDir:     okfOutDir,
		domainName: domainName,
		dbPath:     dbPath,
		configPath: viper.ConfigFileUsed(),
		nodeDefs:   nodeDefs,
		edgeDefs:   edgeDefs,
		nodes:      nodes,
		edges:      edges,
		now:        time.Now().UTC(),
	}
	if err := b.build(); err != nil {
		return err
	}

	log.Printf("\nDone. Bundle written to %s/ — `cat`-readable, git-shippable OKF v%s.", okfOutDir, okfVersion)
	return nil
}

// --- Reading the graph -------------------------------------------------------

type okfNode struct {
	ID         string
	Type       string
	Properties map[string]any
	Semantic   string
	Lat, Lng   *float64
	ValidFrom  *time.Time
}

func readOKFNodes(db *store.DB) ([]okfNode, error) {
	rows, err := db.RawQuery(`
		SELECT node_id, node_type, properties::VARCHAR, COALESCE(semantic_text, ''),
		       latitude, longitude, valid_from
		FROM Nodes_Base
		WHERE is_current = TRUE
		ORDER BY node_type, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]okfNode, 0, 1024)
	for rows.Next() {
		var nid, ntype, props, sem string
		var lat, lng *float64
		var vf sql.NullTime
		if err := rows.Scan(&nid, &ntype, &props, &sem, &lat, &lng, &vf); err != nil {
			return nil, err
		}
		var pmap map[string]any
		if props != "" {
			if err := json.Unmarshal([]byte(props), &pmap); err != nil {
				pmap = map[string]any{}
			}
		}
		n := okfNode{ID: nid, Type: ntype, Properties: pmap, Semantic: sem, Lat: lat, Lng: lng}
		if vf.Valid {
			t := vf.Time.UTC()
			n.ValidFrom = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- Bundle assembly ---------------------------------------------------------

type okfBundle struct {
	outDir     string
	domainName string
	dbPath     string
	configPath string
	nodeDefs   map[string]domain.EntityDef
	edgeDefs   map[string]domain.EntityDef
	nodes      []okfNode
	edges      []edgeOut
	now        time.Time

	// Derived during build.
	nodeIndex map[string]*okfNode // id -> node
	nodePath  map[string]string   // id -> bundle-relative path, only for emitted nodes
	nodeTitle map[string]string   // id -> display title (all nodes)
	typeOrder []string            // node types in stable order
	byType    map[string][]*okfNode
	emitted   map[string][]*okfNode // per type, after filter + cap
	relStats  map[string]*relStat   // relationship_type -> stats
}

type relStat struct {
	count       int
	sourceTypes map[string]bool
	targetTypes map[string]bool
}

func (b *okfBundle) build() error {
	b.index()

	if err := os.MkdirAll(b.outDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	wantFull := okfMode == "full" || okfMode == "both"
	wantCatalog := okfMode == "catalog" || okfMode == "both"

	if wantFull {
		if err := b.writeNodeDocs(); err != nil {
			return err
		}
	}
	if wantCatalog {
		if err := b.writeCatalog(); err != nil {
			return err
		}
	}
	if err := b.writeRootIndex(wantFull, wantCatalog); err != nil {
		return err
	}
	if err := b.writeLog(wantFull, wantCatalog); err != nil {
		return err
	}

	dbName := ""
	configName := ""
	if okfIncludeDB {
		var err error
		if dbName, configName, err = b.copyArtifacts(); err != nil {
			return err
		}
	}
	if okfSkill {
		if err := b.writeSkill(dbName, configName); err != nil {
			return err
		}
	}
	return nil
}

// copyArtifacts copies the DuckDB database and the domain config into the bundle
// so it can be queried in place. Returns their bundle-relative basenames.
func (b *okfBundle) copyArtifacts() (dbName, configName string, err error) {
	if b.dbPath != "" {
		dbName = filepath.Base(b.dbPath)
		if err = copyFile(filepath.Join(b.outDir, dbName), b.dbPath); err != nil {
			return "", "", fmt.Errorf("copying database: %w", err)
		}
		log.Printf("Wrote %s (database copy)", dbName)
	}
	if b.configPath != "" {
		configName = filepath.Base(b.configPath)
		if err = copyFile(filepath.Join(b.outDir, configName), b.configPath); err != nil {
			return "", "", fmt.Errorf("copying config: %w", err)
		}
		log.Printf("Wrote %s (config copy)", configName)
	}
	return dbName, configName, nil
}

// index builds the lookup maps, applies the node-type filter and per-type cap,
// and computes the bundle-relative path of every emitted node so cross-links can
// resolve in a single subsequent pass.
func (b *okfBundle) index() {
	b.nodeIndex = make(map[string]*okfNode, len(b.nodes))
	b.nodeTitle = make(map[string]string, len(b.nodes))
	b.byType = make(map[string][]*okfNode)
	for i := range b.nodes {
		n := &b.nodes[i]
		b.nodeIndex[n.ID] = n
		b.nodeTitle[n.ID] = b.titleFor(n)
		if _, seen := b.byType[n.Type]; !seen {
			b.typeOrder = append(b.typeOrder, n.Type)
		}
		b.byType[n.Type] = append(b.byType[n.Type], n)
	}
	sort.Strings(b.typeOrder)

	// Relationship statistics (used by the catalog).
	b.relStats = make(map[string]*relStat)
	for _, e := range b.edges {
		rs := b.relStats[e.Type]
		if rs == nil {
			rs = &relStat{sourceTypes: map[string]bool{}, targetTypes: map[string]bool{}}
			b.relStats[e.Type] = rs
		}
		rs.count++
		if sn := b.nodeIndex[e.Source]; sn != nil {
			rs.sourceTypes[sn.Type] = true
		}
		if tn := b.nodeIndex[e.Target]; tn != nil {
			rs.targetTypes[tn.Type] = true
		}
	}

	// Apply the node-type filter.
	var keep map[string]bool
	if strings.TrimSpace(okfNodeTypes) != "" {
		keep = map[string]bool{}
		for _, t := range strings.Split(okfNodeTypes, ",") {
			if t = strings.TrimSpace(t); t != "" {
				keep[t] = true
			}
		}
	}

	// Decide emitted nodes (filter + cap) and assign collision-free paths.
	// Paths are only assigned when per-node docs will actually be written
	// (full/both), so cross-links in catalog-only mode degrade to plain text
	// instead of pointing at files that don't exist.
	writeNodeFiles := okfMode == "full" || okfMode == "both"
	b.emitted = make(map[string][]*okfNode)
	b.nodePath = make(map[string]string)
	for _, t := range b.typeOrder {
		if keep != nil && !keep[t] {
			continue
		}
		group := b.byType[t]
		if okfMaxPerType > 0 && len(group) > okfMaxPerType {
			group = group[:okfMaxPerType]
		}
		dir := sanitizeFilename(t)
		used := map[string]bool{}
		for _, n := range group {
			name := uniqueName(used, sanitizeFilename(n.ID))
			if writeNodeFiles {
				b.nodePath[n.ID] = "/" + dir + "/" + name + ".md"
			}
		}
		b.emitted[t] = group
	}
}

func (b *okfBundle) writeNodeDocs() error {
	for _, t := range b.typeOrder {
		group, ok := b.emitted[t]
		if !ok || len(group) == 0 {
			continue
		}
		dir := filepath.Join(b.outDir, sanitizeFilename(t))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		var idx strings.Builder
		fmt.Fprintf(&idx, "# %s\n\n", t)
		for _, n := range group {
			path := b.nodePath[n.ID]
			rel := filepath.Base(path)
			desc := b.descriptionFor(n)
			fmt.Fprintf(&idx, "* [%s](%s)%s\n", mdInline(b.nodeTitle[n.ID]), rel, descSuffix(desc))
			if err := b.writeNodeDoc(n); err != nil {
				return err
			}
		}
		if total := len(b.byType[t]); total > len(group) {
			fmt.Fprintf(&idx, "\n_Showing %d of %d %s concepts (capped by --max-per-type)._\n", len(group), total, t)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(idx.String()), 0o644); err != nil {
			return err
		}
		log.Printf("Wrote %s/ (%d concepts)", sanitizeFilename(t), len(group))
	}
	return nil
}

func (b *okfBundle) writeNodeDoc(n *okfNode) error {
	fm := okfFrontmatter{
		Type:        n.Type,
		Title:       b.titleFor(n),
		Description: b.descriptionFor(n),
		Tags:        []string{n.Type},
		NodeID:      n.ID,
		NodeType:    n.Type,
	}
	if n.ValidFrom != nil {
		fm.Timestamp = n.ValidFrom.Format(time.RFC3339)
	}

	var body strings.Builder
	b.writeProperties(&body, n)
	b.writeRelationships(&body, n)
	if n.Semantic != "" {
		fmt.Fprintf(&body, "# Semantic Text\n\n%s\n\n", n.Semantic)
	}

	doc := renderDoc(fm, body.String())
	path := filepath.Join(b.outDir, filepath.FromSlash(strings.TrimPrefix(b.nodePath[n.ID], "/")))
	return os.WriteFile(path, []byte(doc), 0o644)
}

func (b *okfBundle) writeProperties(w *strings.Builder, n *okfNode) {
	keys := make([]string, 0, len(n.Properties))
	for k := range n.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 && n.Lat == nil {
		return
	}
	w.WriteString("# Properties\n\n")
	w.WriteString("| Field | Value |\n|-------|-------|\n")
	for _, k := range keys {
		fmt.Fprintf(w, "| `%s` | %s |\n", k, mdCell(valToString(n.Properties[k])))
	}
	if n.Lat != nil && n.Lng != nil {
		fmt.Fprintf(w, "| `latitude` | %s |\n", strconv.FormatFloat(*n.Lat, 'f', -1, 64))
		fmt.Fprintf(w, "| `longitude` | %s |\n", strconv.FormatFloat(*n.Lng, 'f', -1, 64))
	}
	w.WriteString("\n")
}

func (b *okfBundle) writeRelationships(w *strings.Builder, n *okfNode) {
	// Group by relationship type and direction.
	type rel struct {
		other    string
		outgoing bool
	}
	byRel := map[string][]rel{}
	order := []string{}
	add := func(rt, other string, outgoing bool) {
		if _, ok := byRel[rt]; !ok {
			order = append(order, rt)
		}
		byRel[rt] = append(byRel[rt], rel{other, outgoing})
	}
	for _, e := range b.edges {
		if e.Source == n.ID {
			add(e.Type, e.Target, true)
		}
		if e.Target == n.ID {
			add(e.Type, e.Source, false)
		}
	}
	if len(order) == 0 {
		return
	}
	sort.Strings(order)
	w.WriteString("# Relationships\n\n")
	for _, rt := range order {
		fmt.Fprintf(w, "## %s\n\n", rt)
		for _, r := range byRel[rt] {
			arrow := "→"
			if !r.outgoing {
				arrow = "←"
			}
			fmt.Fprintf(w, "* %s %s\n", arrow, b.linkTo(r.other))
		}
		w.WriteString("\n")
	}
}

// linkTo renders a markdown link to another node when it was emitted, otherwise
// plain text (a tolerated broken link would also be valid, but plain text is
// clearer when the target was intentionally dropped).
func (b *okfBundle) linkTo(id string) string {
	title := b.nodeTitle[id]
	if title == "" {
		title = id
	}
	if path, ok := b.nodePath[id]; ok {
		return fmt.Sprintf("[%s](%s)", mdInline(title), path)
	}
	return mdInline(title)
}

// --- Catalog -----------------------------------------------------------------

func (b *okfBundle) writeCatalog() error {
	dir := filepath.Join(b.outDir, "catalog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	var nodeIdx, relIdx strings.Builder

	// Node type concepts.
	for _, t := range b.typeOrder {
		def := b.nodeDefs[t]
		fm := okfFrontmatter{
			Type:        "Node Type",
			Title:       t,
			Description: fmt.Sprintf("Node type %q — %d current instances in the %s graph.", t, len(b.byType[t]), b.domainName),
			Tags:        []string{"schema", "node-type"},
			NodeType:    t,
		}
		var body strings.Builder
		b.writeTypeFields(&body, def)
		b.writeTypeRelationships(&body, t)
		b.writeTypeExamples(&body, t)
		name := sanitizeFilename(t)
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(renderDoc(fm, body.String())), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(&nodeIdx, "* [%s](%s.md) - %d instances\n", mdInline(t), name, len(b.byType[t]))
	}

	// Relationship type concepts (union of config + observed edges).
	relTypes := map[string]bool{}
	for rt := range b.edgeDefs {
		relTypes[rt] = true
	}
	for rt := range b.relStats {
		relTypes[rt] = true
	}
	relOrder := make([]string, 0, len(relTypes))
	for rt := range relTypes {
		relOrder = append(relOrder, rt)
	}
	sort.Strings(relOrder)
	for _, rt := range relOrder {
		rs := b.relStats[rt]
		count := 0
		var srcs, tgts []string
		if rs != nil {
			count = rs.count
			srcs, tgts = sortedKeys(rs.sourceTypes), sortedKeys(rs.targetTypes)
		}
		fm := okfFrontmatter{
			Type:        "Relationship Type",
			Title:       rt,
			Description: fmt.Sprintf("Relationship %q — %d current edges.", rt, count),
			Tags:        []string{"schema", "relationship-type"},
		}
		var body strings.Builder
		body.WriteString("# Connects\n\n")
		fmt.Fprintf(&body, "* **Source types:** %s\n", joinOrDash(srcs))
		fmt.Fprintf(&body, "* **Target types:** %s\n", joinOrDash(tgts))
		fmt.Fprintf(&body, "* **Edge count:** %d\n\n", count)
		name := sanitizeFilename(rt)
		if err := os.WriteFile(filepath.Join(dir, "rel_"+name+".md"), []byte(renderDoc(fm, body.String())), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(&relIdx, "* [%s](rel_%s.md) - %d edges\n", mdInline(rt), name, count)
	}

	var idx strings.Builder
	idx.WriteString("# Node Types\n\n")
	idx.WriteString(nodeIdx.String())
	idx.WriteString("\n# Relationship Types\n\n")
	idx.WriteString(relIdx.String())
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(idx.String()), 0o644); err != nil {
		return err
	}
	log.Printf("Wrote catalog/ (%d node types, %d relationship types)", len(b.typeOrder), len(relOrder))
	return nil
}

func (b *okfBundle) writeTypeFields(w *strings.Builder, def domain.EntityDef) {
	if len(def.Mappings) == 0 {
		return
	}
	w.WriteString("# Fields\n\n")
	w.WriteString("| Field | Key | Source |\n|-------|-----|--------|\n")
	for _, m := range def.Mappings {
		key := ""
		if m.IsKey {
			key = "✓"
		}
		fmt.Fprintf(w, "| `%s` | %s | `%s` |\n", m.TargetField, key, m.SourceField)
	}
	w.WriteString("\n")
	if len(def.SemanticFields) > 0 {
		w.WriteString("# Semantic Fields\n\n")
		w.WriteString("Fields concatenated to form the embedding text:\n\n")
		for _, f := range def.SemanticFields {
			fmt.Fprintf(w, "* `%s`\n", f)
		}
		w.WriteString("\n")
	}
}

func (b *okfBundle) writeTypeRelationships(w *strings.Builder, t string) {
	var out, in []string
	for rt, rs := range b.relStats {
		if rs.sourceTypes[t] {
			out = append(out, fmt.Sprintf("[%s](rel_%s.md)", mdInline(rt), sanitizeFilename(rt)))
		}
		if rs.targetTypes[t] {
			in = append(in, fmt.Sprintf("[%s](rel_%s.md)", mdInline(rt), sanitizeFilename(rt)))
		}
	}
	if len(out) == 0 && len(in) == 0 {
		return
	}
	sort.Strings(out)
	sort.Strings(in)
	w.WriteString("# Relationships\n\n")
	if len(out) > 0 {
		fmt.Fprintf(w, "* **Outgoing:** %s\n", strings.Join(out, ", "))
	}
	if len(in) > 0 {
		fmt.Fprintf(w, "* **Incoming:** %s\n", strings.Join(in, ", "))
	}
	w.WriteString("\n")
}

func (b *okfBundle) writeTypeExamples(w *strings.Builder, t string) {
	emitted := b.emitted[t]
	if len(emitted) == 0 {
		return
	}
	limit := 10
	if len(emitted) < limit {
		limit = len(emitted)
	}
	w.WriteString("# Examples\n\n")
	for _, n := range emitted[:limit] {
		fmt.Fprintf(w, "* %s\n", b.linkTo(n.ID))
	}
	w.WriteString("\n")
}

// --- Root index & log --------------------------------------------------------

func (b *okfBundle) writeRootIndex(wantFull, wantCatalog bool) error {
	title := b.domainName
	if title == "" {
		title = "Knowledge"
	}
	header := fmt.Sprintf("---\nokf_version: \"%s\"\n---\n\n# %s knowledge bundle\n\n", okfVersion, title)

	var sb strings.Builder
	sb.WriteString(header)
	if wantCatalog {
		sb.WriteString("# Catalog\n\n")
		sb.WriteString("* [catalog](catalog/index.md) - node-type and relationship-type schema overview\n\n")
	}
	if wantFull {
		sb.WriteString("# Node Types\n\n")
		for _, t := range b.typeOrder {
			group, ok := b.emitted[t]
			if !ok || len(group) == 0 {
				continue
			}
			fmt.Fprintf(&sb, "* [%s](%s/index.md) - %d concepts\n", mdInline(t), sanitizeFilename(t), len(group))
		}
		sb.WriteString("\n")
	}
	return os.WriteFile(filepath.Join(b.outDir, "index.md"), []byte(sb.String()), 0o644)
}

func (b *okfBundle) writeLog(wantFull, wantCatalog bool) error {
	emittedNodes := 0
	for _, g := range b.emitted {
		emittedNodes += len(g)
	}
	var sb strings.Builder
	sb.WriteString("# Bundle Update Log\n\n")
	fmt.Fprintf(&sb, "## %s\n", b.now.Format("2006-01-02"))
	fmt.Fprintf(&sb, "* **Initialization**: Generated OKF v%s bundle from the %s knowledge graph (mode: %s).\n",
		okfVersion, b.domainName, okfMode)
	if wantFull {
		fmt.Fprintf(&sb, "* **Creation**: Wrote %d node concepts across %d types.\n", emittedNodes, len(b.typeOrder))
	}
	if wantCatalog {
		fmt.Fprintf(&sb, "* **Creation**: Wrote catalog for %d node types and %d relationship types.\n",
			len(b.typeOrder), len(b.relStats))
	}
	return os.WriteFile(filepath.Join(b.outDir, "log.md"), []byte(sb.String()), 0o644)
}

// --- SKILL.md ----------------------------------------------------------------

// writeSkill emits a SKILL.md that turns the bundle into a self-describing agent
// skill: it explains how to use the browsable OKF markdown for orientation and
// (when included) the DuckDB database for precise hybrid retrieval. dbName and
// configName are the bundled artifact basenames, or "" when not included.
func (b *okfBundle) writeSkill(dbName, configName string) error {
	name := skillName(b.domainName)
	title := b.domainName
	if title == "" {
		title = "knowledge"
	}

	var nodeCount int
	for _, g := range b.byType {
		nodeCount += len(g)
	}

	desc := fmt.Sprintf(
		"Explore and query the %s knowledge graph (%d entities across %d types, %d relationships). "+
			"Pairs browsable OKF markdown concepts with a DuckDB database for hybrid vector + lexical + graph search. "+
			"Use when answering questions about %s, looking up entities and how they relate, or running graph/semantic queries.",
		title, nodeCount, len(b.typeOrder), len(b.edges), title)

	// Ordered frontmatter: skills conventionally lead with name/description; the
	// type field keeps the file OKF-conformant (skill loaders ignore unknown keys).
	fm := struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Type        string `yaml:"type"`
	}{Name: name, Description: desc, Type: "Skill"}
	y, _ := yaml.Marshal(fm)

	var s strings.Builder
	s.WriteString("---\n")
	s.Write(y)
	s.WriteString("---\n\n")

	fmt.Fprintf(&s, "# %s knowledge graph\n\n", title)
	s.WriteString("This bundle is an [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md) ")
	s.WriteString("(OKF v" + okfVersion + ") corpus describing a temporal knowledge graph. ")
	s.WriteString("Markdown gives you human/agent-readable orientation; ")
	if dbName != "" {
		s.WriteString("the bundled DuckDB database gives you precise, queryable retrieval.\n\n")
	} else {
		s.WriteString("pair it with the source DuckDB database for precise, queryable retrieval.\n\n")
	}

	// What's in the bundle.
	s.WriteString("## What's in this bundle\n\n")
	s.WriteString("* `index.md` — root listing of everything available (start here).\n")
	s.WriteString("* `catalog/` — one concept per node type and relationship type: fields, semantic fields, counts, examples.\n")
	s.WriteString("* `<NodeType>/` — one markdown concept per entity, with its properties and relationships as cross-links.\n")
	s.WriteString("* `log.md` — generation history.\n")
	if dbName != "" {
		fmt.Fprintf(&s, "* `%s` — the knowledge graph database (DuckDB).\n", dbName)
	}
	if configName != "" {
		fmt.Fprintf(&s, "* `%s` — the domain config (node/edge definitions, embedding model).\n", configName)
	}
	s.WriteString("\n")

	// Node / relationship types present.
	s.WriteString("## Entity types\n\n")
	for _, t := range b.typeOrder {
		fmt.Fprintf(&s, "* **%s** — %d (`catalog/%s.md`)\n", t, len(b.byType[t]), sanitizeFilename(t))
	}
	s.WriteString("\n")
	if len(b.relStats) > 0 {
		relOrder := make([]string, 0, len(b.relStats))
		for rt := range b.relStats {
			relOrder = append(relOrder, rt)
		}
		sort.Strings(relOrder)
		s.WriteString("Relationship types: ")
		s.WriteString("`" + strings.Join(relOrder, "`, `") + "`\n\n")
	}

	// How to use.
	s.WriteString("## How to use it\n\n")
	s.WriteString("1. **Orient** — read `index.md`, then the relevant `catalog/<Type>.md` to learn the shape of the data.\n")
	s.WriteString("2. **Browse** — open individual `<NodeType>/<id>.md` concepts and follow relationship links between them.\n")
	if dbName != "" {
		s.WriteString("3. **Query** — for precise lookups, joins, or aggregation, query the database directly (below).\n\n")
	} else {
		s.WriteString("3. **Query** — for precise lookups, point the queries below at the source database.\n\n")
	}

	// Querying the DB.
	example := b.firstEmittedID()
	exampleType := b.firstEmittedType()
	dbRef := dbName
	if dbRef == "" {
		dbRef = filepath.Base(b.dbPath)
	}
	s.WriteString("## Querying the database\n\n")
	s.WriteString("The graph is stored in two DuckDB tables, `Nodes_Base` and `Edges_Base`, ")
	s.WriteString("with SCD Type 2 temporal tracking (filter `is_current = TRUE` for the present state).\n\n")
	s.WriteString("Raw DuckDB SQL (portable — needs only the `duckdb` CLI):\n\n")
	s.WriteString("```sql\n")
	fmt.Fprintf(&s, "-- entities of a type\nSELECT node_id, properties FROM Nodes_Base\nWHERE is_current AND node_type = '%s' LIMIT 20;\n\n", exampleType)
	fmt.Fprintf(&s, "-- a node's relationships\nSELECT relationship_type, target_id FROM Edges_Base\nWHERE is_current AND source_id = '%s';\n", example)
	s.WriteString("```\n\n")
	s.WriteString("Hybrid search (BM25 + vector + graph) via the `cbi` CLI, run from this directory:\n\n")
	s.WriteString("```bash\n")
	if configName != "" {
		fmt.Fprintf(&s, "cbi query --config %s --text \"your question\" --limit 10\n", configName)
		fmt.Fprintf(&s, "cbi graph --config %s --sql \"FROM GRAPH_TABLE(domain_graph MATCH (a:\\\"node\\\")-[e:\\\"edge\\\"]->(b:\\\"node\\\") COLUMNS (a.node_id, e.relationship_type, b.node_id)) LIMIT 10\"\n", configName)
	} else {
		s.WriteString("cbi query --text \"your question\" --limit 10\n")
	}
	s.WriteString("```\n\n")
	s.WriteString("> Vector/semantic search needs the embedding endpoint from the domain config to be reachable; ")
	s.WriteString("lexical and graph queries work offline against the database alone.\n")

	path := filepath.Join(b.outDir, "SKILL.md")
	if err := os.WriteFile(path, []byte(s.String()), 0o644); err != nil {
		return err
	}
	log.Printf("Wrote SKILL.md (skill: %s)", name)
	return nil
}

func (b *okfBundle) firstEmittedID() string {
	for _, t := range b.typeOrder {
		if g := b.emitted[t]; len(g) > 0 {
			return g[0].ID
		}
	}
	if len(b.nodes) > 0 {
		return b.nodes[0].ID
	}
	return "node:example"
}

func (b *okfBundle) firstEmittedType() string {
	for _, t := range b.typeOrder {
		if len(b.emitted[t]) > 0 {
			return t
		}
	}
	if len(b.typeOrder) > 0 {
		return b.typeOrder[0]
	}
	return "Node"
}

// --- Derivation helpers ------------------------------------------------------

// titleFor derives a display name from the first non-empty, non-key display
// field declared in the domain config, falling back to the node ID.
func (b *okfBundle) titleFor(n *okfNode) string {
	def := b.nodeDefs[n.Type]
	for _, m := range def.Mappings {
		if m.IsKey {
			continue
		}
		if v, ok := n.Properties[m.TargetField]; ok {
			if s := strings.TrimSpace(valToString(v)); s != "" {
				return s
			}
		}
	}
	return n.ID
}

func (b *okfBundle) descriptionFor(n *okfNode) string {
	s := strings.TrimSpace(n.Semantic)
	if s == "" {
		return ""
	}
	// First line, collapsed and truncated to a single sentence-ish summary.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	const max = 200
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// --- Frontmatter & markdown rendering ---------------------------------------

type okfFrontmatter struct {
	Type        string   `yaml:"type"`
	Title       string   `yaml:"title,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Resource    string   `yaml:"resource,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	Timestamp   string   `yaml:"timestamp,omitempty"`
	NodeID      string   `yaml:"node_id,omitempty"`
	NodeType    string   `yaml:"node_type,omitempty"`
}

func renderDoc(fm okfFrontmatter, body string) string {
	y, err := yaml.Marshal(fm)
	if err != nil {
		// Type is the only required field; degrade gracefully rather than fail.
		y = []byte(fmt.Sprintf("type: %q\n", fm.Type))
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(y)
	sb.WriteString("---\n\n")
	sb.WriteString(body)
	return sb.String()
}

func valToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		bs, _ := json.Marshal(t)
		return string(bs)
	}
}

// mdCell escapes a value for use inside a markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// mdInline escapes a value for use as inline markdown link text.
func mdInline(s string) string {
	s = strings.ReplaceAll(s, "[", "\\[")
	s = strings.ReplaceAll(s, "]", "\\]")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func descSuffix(desc string) string {
	if desc == "" {
		return ""
	}
	return " - " + mdInline(desc)
}

// sanitizeFilename maps any character outside [A-Za-z0-9._-] to '_' so node IDs
// like "biz:12345:1" become safe, portable path segments.
func sanitizeFilename(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		out = "node"
	}
	return out
}

func uniqueName(used map[string]bool, base string) string {
	name := base
	for i := 2; used[name]; i++ {
		name = base + "_" + strconv.Itoa(i)
	}
	used[name] = true
	return name
}

// skillName derives a valid kebab-case skill name from the domain name.
func skillName(domain string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(domain) {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "knowledge"
	}
	return out + "-graph"
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinOrDash(s []string) string {
	if len(s) == 0 {
		return "—"
	}
	return strings.Join(s, ", ")
}

func init() {
	okfCmd.Flags().StringVarP(&okfOutDir, "output", "o", "okf", "output directory for the OKF bundle")
	okfCmd.Flags().StringVar(&okfMode, "mode", "both", "what to emit: full | catalog | both")
	okfCmd.Flags().IntVar(&okfMaxPerType, "max-per-type", 0, "cap per-node documents written per node type (0 = unlimited)")
	okfCmd.Flags().StringVar(&okfNodeTypes, "node-types", "", "comma-separated list of node types to include (default: all)")
	okfCmd.Flags().BoolVar(&okfSkill, "skill", false, "emit a SKILL.md so the bundle doubles as a self-describing agent skill")
	okfCmd.Flags().BoolVar(&okfIncludeDB, "include-db", false, "copy the DuckDB database and domain config into the bundle")
	generateCmd.AddCommand(okfCmd)
}
