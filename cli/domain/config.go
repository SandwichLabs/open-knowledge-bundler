package domain

// FieldMapping describes how a raw data field maps into the graph schema.
type FieldMapping struct {
	SourceField string `yaml:"source_field"`
	TargetField string `yaml:"target_field"`
	IsKey       bool   `yaml:"is_key,omitempty"`
}

// EntityDef defines a node or edge type and how to populate it from raw data.
type EntityDef struct {
	SemanticFields []string       `yaml:"semantic_fields,omitempty"` // fields concatenated for embedding
	Mappings       []FieldMapping `yaml:"mappings"`
}

// DomainConfig dictates how the CLI maps raw data to the graph.
type DomainConfig struct {
	DomainName      string               `yaml:"domain_name"`
	EmbeddingDim    int                  `yaml:"embedding_dim"`
	EmbeddingModel  string               `yaml:"embedding_model"`
	EndpointURL     string               `yaml:"endpoint_url"`
	DatabasePath    string               `yaml:"database_path"`
	Ontology        *Ontology            `yaml:"ontology,omitempty"`
	NodeDefinitions map[string]EntityDef `yaml:"node_definitions"`
	EdgeDefinitions map[string]EntityDef `yaml:"edge_definitions"`
}

// TypeDef is a single entity (node) type in an extraction ontology.
type TypeDef struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// RelationDef is a single canonical relation in an extraction ontology. The
// relation is directional: edges run source_type -> target_type. Inverse, when
// set, names the canonical relation that expresses the same fact in the other
// direction (e.g. SYMPTOM_OF is the inverse of HAS_SYMPTOM); the normalizer
// rewrites inverse mentions to the canonical name and swaps endpoints.
type RelationDef struct {
	Name        string `yaml:"name" json:"name"`
	SourceType  string `yaml:"source_type" json:"source_type"`
	TargetType  string `yaml:"target_type" json:"target_type"`
	Inverse     string `yaml:"inverse,omitempty" json:"inverse,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Ontology is the closed entity-type + relation vocabulary that constrains LLM
// extraction. It is proposed by a bootstrap pass, persisted in domain.yaml, and
// editable by the user before a full run.
type Ontology struct {
	EntityTypes []TypeDef     `yaml:"entity_types"`
	Relations   []RelationDef `yaml:"relations"`
}

// EntityTypeNames returns the entity type names (the enum used in extraction).
func (o *Ontology) EntityTypeNames() []string {
	names := make([]string, len(o.EntityTypes))
	for i, t := range o.EntityTypes {
		names[i] = t.Name
	}
	return names
}

// RelationNames returns the canonical relation names (the enum used in extraction).
func (o *Ontology) RelationNames() []string {
	names := make([]string, len(o.Relations))
	for i, r := range o.Relations {
		names[i] = r.Name
	}
	return names
}
