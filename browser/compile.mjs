#!/usr/bin/env node

/**
 * Compile a DuckDB knowledge graph into a self-contained browser search experience.
 *
 * Usage:
 *   node compile.mjs --config path/to/domain.yaml [--output dist/]
 *
 * Reads domain.yaml to find the database_path, queries DuckDB for current nodes
 * and edges, and writes:
 *   - graph.json   (nodes, edges, domain config for UI generation)
 *   - embeddings.bin (flat Float32Array of node embeddings)
 *   - index.html   (self-contained search app)
 */

import { DuckDBInstance } from '@duckdb/node-api';
import { readFileSync, writeFileSync, mkdirSync, copyFileSync } from 'fs';
import { parse as parseYAML } from 'yaml';
import { parseArgs } from 'util';
import { resolve, dirname, join } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------------------
// CLI argument parsing
// ---------------------------------------------------------------------------
const { values: args } = parseArgs({
  options: {
    config: { type: 'string', short: 'c' },
    output: { type: 'string', short: 'o', default: 'dist' },
  },
});

if (!args.config) {
  console.error('Usage: node compile.mjs --config <domain.yaml> [--output <dir>]');
  process.exit(1);
}

// ---------------------------------------------------------------------------
// Load domain configuration
// ---------------------------------------------------------------------------
const configPath = resolve(args.config);
const configDir = dirname(configPath);
const config = parseYAML(readFileSync(configPath, 'utf8'));
const dbPath = resolve(configDir, config.database_path);

console.log(`Domain:   ${config.domain_name}`);
console.log(`Database: ${dbPath}`);
console.log(`Dim:      ${config.embedding_dim}`);

// ---------------------------------------------------------------------------
// Derive display_fields and semantic_fields from domain config
// ---------------------------------------------------------------------------
function deriveNodeConfig(nodeDefs) {
  const result = {};
  for (const [typeName, def] of Object.entries(nodeDefs)) {
    const displayFields = def.mappings
      .filter(m => !m.is_key)
      .map(m => m.target_field);
    result[typeName] = {
      semantic_fields: def.semantic_fields || [],
      display_fields: displayFields,
    };
  }
  return result;
}

function deriveEdgeConfig(edgeDefs) {
  const result = {};
  for (const typeName of Object.keys(edgeDefs)) {
    result[typeName] = {};
  }
  return result;
}

// ---------------------------------------------------------------------------
// Query DuckDB
// ---------------------------------------------------------------------------
async function queryDB() {
  const instance = await DuckDBInstance.create(dbPath, { access_mode: 'READ_ONLY' });
  const conn = await instance.connect();

  // --- Nodes ---
  const nodeResult = await conn.run(`
    SELECT node_id, node_type, properties, semantic_text, embedding,
           latitude, longitude
    FROM Nodes_Base
    WHERE is_current = TRUE
    ORDER BY node_id
  `);

  const nodes = [];
  const embeddingChunks = [];
  const dim = config.embedding_dim;

  const nodeRows = await nodeResult.getRows();
  for (const row of nodeRows) {
    const [nodeId, nodeType, propertiesRaw, semanticText, embeddingRaw, lat, lng] = row;

    let properties;
    if (typeof propertiesRaw === 'string') {
      try { properties = JSON.parse(propertiesRaw); } catch { properties = {}; }
    } else if (propertiesRaw && typeof propertiesRaw === 'object') {
      properties = propertiesRaw;
    } else {
      properties = {};
    }

    nodes.push({
      id: nodeId,
      type: nodeType,
      properties,
      semantic_text: semanticText || '',
      ...(lat != null && lng != null ? { lat, lng } : {}),
    });

    // Extract embedding floats — @duckdb/node-api returns arrays as {items: [...]}
    let embArr;
    if (embeddingRaw && embeddingRaw.items) {
      embArr = embeddingRaw.items;
    } else if (embeddingRaw && Array.isArray(embeddingRaw)) {
      embArr = embeddingRaw;
    } else {
      embArr = new Array(dim).fill(0);
    }
    embeddingChunks.push(...embArr.slice(0, dim));
  }

  // --- Edges ---
  const edgeResult = await conn.run(`
    SELECT edge_id, source_id, target_id, relationship_type, weight
    FROM Edges_Base
    WHERE is_current = TRUE
    ORDER BY edge_id
  `);

  const edges = [];
  const edgeRows = await edgeResult.getRows();
  for (const row of edgeRows) {
    const [edgeId, sourceId, targetId, relType, weight] = row;
    edges.push({
      id: edgeId,
      source: sourceId,
      target: targetId,
      type: relType,
      weight: weight ?? 1.0,
    });
  }

  conn.closeSync();

  return { nodes, edges, embeddings: new Float32Array(embeddingChunks) };
}

// ---------------------------------------------------------------------------
// Write output
// ---------------------------------------------------------------------------
async function main() {
  const outDir = resolve(args.output);
  mkdirSync(outDir, { recursive: true });

  console.log('Querying database...');
  const { nodes, edges, embeddings } = await queryDB();

  console.log(`Nodes: ${nodes.length}  Edges: ${edges.length}`);
  console.log(`Embeddings: ${embeddings.length} floats (${(embeddings.byteLength / 1024).toFixed(1)} KB)`);

  // graph.json
  const graphData = {
    meta: {
      domain_name: config.domain_name,
      embedding_dim: config.embedding_dim,
      node_count: nodes.length,
      edge_count: edges.length,
    },
    config: {
      node_definitions: deriveNodeConfig(config.node_definitions),
      edge_definitions: deriveEdgeConfig(config.edge_definitions),
    },
    nodes,
    edges,
  };

  const graphPath = join(outDir, 'graph.json');
  writeFileSync(graphPath, JSON.stringify(graphData));
  console.log(`Wrote ${graphPath}`);

  // embeddings.bin
  const embPath = join(outDir, 'embeddings.bin');
  writeFileSync(embPath, Buffer.from(embeddings.buffer));
  console.log(`Wrote ${embPath} (${(embeddings.byteLength / 1024).toFixed(1)} KB)`);

  // index.html
  const templatePath = join(__dirname, 'template.html');
  const htmlPath = join(outDir, 'index.html');
  copyFileSync(templatePath, htmlPath);
  console.log(`Wrote ${htmlPath}`);

  console.log('\nDone! Serve dist/ with any static file server:');
  console.log('  npx serve dist/');
}

main().catch(err => {
  console.error('Compile failed:', err);
  process.exit(1);
});
