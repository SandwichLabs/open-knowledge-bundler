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
	NodeDefinitions map[string]EntityDef `yaml:"node_definitions"`
	EdgeDefinitions map[string]EntityDef `yaml:"edge_definitions"`
}
