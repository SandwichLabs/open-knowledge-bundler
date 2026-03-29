#!/usr/bin/env npx tsx
/**
 * to_ndjson.ts — Convert Chicago business data from DuckDB into NDJSON files
 * compatible with the `cbi ingest` command.
 *
 * Produces:
 *   out/nodes.ndjson   — Business, Neighborhood, Ward, Activity, LicenseType nodes
 *   out/edges.ndjson   — LOCATED_IN, IN_WARD, HAS_ACTIVITY, LICENSED_AS, SAME_ACCOUNT, SAME_ENTITY, NEAR edges
 *
 * Usage:
 *   npx tsx data/to_ndjson.ts [--db chi-city-data.duckdb] [--out out/] [--near-meters 200]
 */

import { DuckDBInstance } from "@duckdb/node-api";
import { writeFile, mkdir } from "node:fs/promises";
import { parseArgs } from "node:util";
import { join } from "node:path";

// ---------------------------------------------------------------------------
// CLI args
// ---------------------------------------------------------------------------
const { values: args } = parseArgs({
  options: {
    db: { type: "string", default: "chi-city-data.duckdb" },
    out: { type: "string", default: "out" },
    "near-meters": { type: "string", default: "200" },
  },
});

const DB_PATH = args.db!;
const OUT_DIR = args.out!;
const NEAR_METERS = Number(args["near-meters"]);

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
interface IngestNode {
  node_id: string;
  node_type: string;
  properties: Record<string, unknown>;
  semantic_text: string;
  latitude?: number;
  longitude?: number;
}

interface IngestEdge {
  edge_id: string;
  source_id: string;
  target_id: string;
  relationship_type: string;
  weight: number;
}

function ndjson(items: unknown[]): string {
  return items.map((i) => JSON.stringify(i)).join("\n") + "\n";
}

// Query helper using the new @duckdb/node-api
async function queryAll(
  conn: any,
  sql: string
): Promise<Record<string, unknown>[]> {
  const result = await conn.run(sql);
  const rows: Record<string, unknown>[] = [];
  const reader = result.getRows();
  // Read column names
  const columnCount = result.columnCount;
  const columnNames: string[] = [];
  for (let i = 0; i < columnCount; i++) {
    columnNames.push(result.columnName(i));
  }
  // Iterate chunks
  for (const chunk of reader) {
    for (const row of chunk.getRows()) {
      const obj: Record<string, unknown> = {};
      for (let i = 0; i < columnNames.length; i++) {
        obj[columnNames[i]] = row.getValueAs(i);
      }
      rows.push(obj);
    }
  }
  return rows;
}

// Simpler query approach using run + getRows
async function query(conn: any, sql: string): Promise<any[]> {
  const stmt = await conn.prepare(sql);
  const result = await stmt.run();
  const rows: any[] = [];

  // Use the result reader API
  let columns: string[] = [];
  const colCount = result.columnCount;
  for (let c = 0; c < colCount; c++) {
    columns.push(result.columnName(c));
  }

  // Materialize all rows
  const chunks = await result.fetchAllChunks();
  for (const chunk of chunks) {
    const rowCount = chunk.rowCount;
    for (let r = 0; r < rowCount; r++) {
      const row: Record<string, unknown> = {};
      for (let c = 0; c < colCount; c++) {
        row[columns[c]] = chunk.getColumnVector(c).getItem(r);
      }
      rows.push(row);
    }
  }
  stmt.close();
  return rows;
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------
async function main() {
  await mkdir(OUT_DIR, { recursive: true });

  console.log(`Opening ${DB_PATH}...`);
  const instance = await DuckDBInstance.create(DB_PATH, { access_mode: "READ_ONLY" });
  const conn = await instance.connect();

  // Install spatial for distance computation
  await conn.run("INSTALL spatial; LOAD spatial;");

  const nodes: IngestNode[] = [];
  const edges: IngestEdge[] = [];

  // =========================================================================
  // 1. BUSINESS NODES
  // =========================================================================
  console.log("Extracting Business nodes...");
  const businesses = await query(
    conn,
    `SELECT
       "ACCOUNT NUMBER" as account_number,
       "SITE NUMBER" as site_number,
       "ID" as license_id,
       "LEGAL NAME" as legal_name,
       "DOING BUSINESS AS NAME" as doing_business_as,
       "ADDRESS" as address,
       "CITY" as city,
       "STATE" as state,
       "ZIP CODE" as zip_code,
       CAST("LATITUDE" AS DOUBLE) as latitude,
       CAST("LONGITUDE" AS DOUBLE) as longitude,
       "PRI_NEIGH" as neighborhood,
       "SEC_NEIGH" as secondary_neighborhood,
       CAST("WARD" AS VARCHAR) as ward,
       "LICENSE CODE" as license_code,
       "LICENSE DESCRIPTION" as license_description,
       "activity" as activity,
       "BUSINESS ACTIVITY ID" as activity_id,
       "LICENSE STATUS" as license_status,
       "DATE ISSUED" as date_issued,
       "LICENSE TERM START DATE" as license_term_start,
       "LICENSE TERM EXPIRATION DATE" as license_term_expiration
     FROM city_businesses`
  );

  console.log(`  ${businesses.length} business records`);

  // Deduplicate by account_number + site_number (a business at a location)
  const bizSeen = new Set<string>();
  for (const b of businesses) {
    const nodeId = `biz:${b.account_number}:${b.site_number}`;
    if (bizSeen.has(nodeId)) continue;
    bizSeen.add(nodeId);

    const lat = b.latitude as number | null;
    const lng = b.longitude as number | null;

    const semanticParts = [
      b.legal_name,
      b.doing_business_as,
      b.activity,
      b.license_description,
      b.address,
      b.neighborhood,
    ].filter(Boolean);

    const node: IngestNode = {
      node_id: nodeId,
      node_type: "Business",
      properties: { ...b },
      semantic_text: semanticParts.join(" | "),
    };
    if (lat && lng && !isNaN(lat) && !isNaN(lng)) {
      node.latitude = lat;
      node.longitude = lng;
    }
    nodes.push(node);
  }
  console.log(`  ${bizSeen.size} unique Business nodes (deduped by account+site)`);

  // =========================================================================
  // 2. NEIGHBORHOOD NODES
  // =========================================================================
  console.log("Extracting Neighborhood nodes...");
  const neighborhoods = await query(
    conn,
    `SELECT
       PRI_NEIGH as name,
       SEC_NEIGH as secondary_name,
       SHAPE_AREA as shape_area,
       SHAPE_LEN as shape_length
     FROM neighborhood_bounds`
  );

  for (const n of neighborhoods) {
    nodes.push({
      node_id: `neighborhood:${n.name}`,
      node_type: "Neighborhood",
      properties: { ...n },
      semantic_text: `${n.name}${n.secondary_name ? " (" + n.secondary_name + ")" : ""} neighborhood, Chicago`,
    });
  }
  console.log(`  ${neighborhoods.length} Neighborhood nodes`);

  // =========================================================================
  // 3. WARD NODES
  // =========================================================================
  console.log("Extracting Ward nodes...");
  const wards = await query(
    conn,
    `SELECT DISTINCT CAST("WARD" AS VARCHAR) as ward
     FROM city_businesses
     WHERE "WARD" IS NOT NULL
     ORDER BY 1`
  );

  for (const w of wards) {
    nodes.push({
      node_id: `ward:${w.ward}`,
      node_type: "Ward",
      properties: { name: `Ward ${w.ward}` },
      semantic_text: `Ward ${w.ward}, Chicago`,
    });
  }
  console.log(`  ${wards.length} Ward nodes`);

  // =========================================================================
  // 4. ACTIVITY NODES
  // =========================================================================
  console.log("Extracting Activity nodes...");
  const activities = await query(
    conn,
    `SELECT DISTINCT
       CAST("BUSINESS ACTIVITY ID" AS VARCHAR) as activity_id,
       "activity" as name
     FROM city_businesses
     WHERE "BUSINESS ACTIVITY ID" IS NOT NULL
     ORDER BY 1`
  );

  for (const a of activities) {
    nodes.push({
      node_id: `activity:${a.activity_id}`,
      node_type: "Activity",
      properties: { activity_id: a.activity_id, name: a.name },
      semantic_text: String(a.name),
    });
  }
  console.log(`  ${activities.length} Activity nodes`);

  // =========================================================================
  // 5. LICENSE TYPE NODES
  // =========================================================================
  console.log("Extracting LicenseType nodes...");
  const licenseTypes = await query(
    conn,
    `SELECT DISTINCT
       CAST("LICENSE CODE" AS VARCHAR) as license_code,
       "LICENSE DESCRIPTION" as name
     FROM city_businesses
     WHERE "LICENSE CODE" IS NOT NULL
     ORDER BY 1`
  );

  for (const lt of licenseTypes) {
    nodes.push({
      node_id: `license:${lt.license_code}`,
      node_type: "LicenseType",
      properties: { license_code: lt.license_code, name: lt.name },
      semantic_text: String(lt.name),
    });
  }
  console.log(`  ${licenseTypes.length} LicenseType nodes`);

  // =========================================================================
  // Write nodes NDJSON
  // =========================================================================
  const nodesPath = join(OUT_DIR, "nodes.ndjson");
  console.log(`\nWriting ${nodes.length} nodes to ${nodesPath}...`);
  await writeFile(nodesPath, ndjson(nodes));

  // =========================================================================
  // 6. LOCATED_IN EDGES
  // =========================================================================
  console.log("\nExtracting LOCATED_IN edges...");
  const locatedIn = await query(
    conn,
    `SELECT DISTINCT
       CAST("ACCOUNT NUMBER" AS VARCHAR) as account_number,
       CAST("SITE NUMBER" AS VARCHAR) as site_number,
       "PRI_NEIGH" as neighborhood
     FROM city_businesses
     WHERE "PRI_NEIGH" IS NOT NULL`
  );

  for (const r of locatedIn) {
    const srcId = `biz:${r.account_number}:${r.site_number}`;
    if (!bizSeen.has(srcId)) continue;
    edges.push({
      edge_id: `located_in:${r.account_number}:${r.site_number}:${r.neighborhood}`,
      source_id: srcId,
      target_id: `neighborhood:${r.neighborhood}`,
      relationship_type: "LOCATED_IN",
      weight: 1.0,
    });
  }
  console.log(`  ${edges.length} LOCATED_IN edges`);

  // =========================================================================
  // 7. IN_WARD EDGES
  // =========================================================================
  console.log("Extracting IN_WARD edges...");
  const edgeCountBefore = edges.length;
  const inWard = await query(
    conn,
    `SELECT DISTINCT
       CAST("ACCOUNT NUMBER" AS VARCHAR) as account_number,
       CAST("SITE NUMBER" AS VARCHAR) as site_number,
       CAST("WARD" AS VARCHAR) as ward
     FROM city_businesses
     WHERE "WARD" IS NOT NULL`
  );

  for (const r of inWard) {
    const srcId = `biz:${r.account_number}:${r.site_number}`;
    if (!bizSeen.has(srcId)) continue;
    edges.push({
      edge_id: `in_ward:${r.account_number}:${r.site_number}:${r.ward}`,
      source_id: srcId,
      target_id: `ward:${r.ward}`,
      relationship_type: "IN_WARD",
      weight: 1.0,
    });
  }
  console.log(`  ${edges.length - edgeCountBefore} IN_WARD edges`);

  // =========================================================================
  // 8. HAS_ACTIVITY EDGES
  // =========================================================================
  console.log("Extracting HAS_ACTIVITY edges...");
  const hasActBefore = edges.length;
  const hasActivity = await query(
    conn,
    `SELECT DISTINCT
       CAST("ACCOUNT NUMBER" AS VARCHAR) as account_number,
       CAST("SITE NUMBER" AS VARCHAR) as site_number,
       CAST("BUSINESS ACTIVITY ID" AS VARCHAR) as activity_id
     FROM city_businesses
     WHERE "BUSINESS ACTIVITY ID" IS NOT NULL`
  );

  for (const r of hasActivity) {
    const srcId = `biz:${r.account_number}:${r.site_number}`;
    if (!bizSeen.has(srcId)) continue;
    edges.push({
      edge_id: `has_activity:${r.account_number}:${r.site_number}:${r.activity_id}`,
      source_id: srcId,
      target_id: `activity:${r.activity_id}`,
      relationship_type: "HAS_ACTIVITY",
      weight: 1.0,
    });
  }
  console.log(`  ${edges.length - hasActBefore} HAS_ACTIVITY edges`);

  // =========================================================================
  // 9. LICENSED_AS EDGES
  // =========================================================================
  console.log("Extracting LICENSED_AS edges...");
  const licBefore = edges.length;
  const licensedAs = await query(
    conn,
    `SELECT DISTINCT
       CAST("ACCOUNT NUMBER" AS VARCHAR) as account_number,
       CAST("SITE NUMBER" AS VARCHAR) as site_number,
       CAST("LICENSE CODE" AS VARCHAR) as license_code
     FROM city_businesses
     WHERE "LICENSE CODE" IS NOT NULL`
  );

  for (const r of licensedAs) {
    const srcId = `biz:${r.account_number}:${r.site_number}`;
    if (!bizSeen.has(srcId)) continue;
    edges.push({
      edge_id: `licensed_as:${r.account_number}:${r.site_number}:${r.license_code}`,
      source_id: srcId,
      target_id: `license:${r.license_code}`,
      relationship_type: "LICENSED_AS",
      weight: 1.0,
    });
  }
  console.log(`  ${edges.length - licBefore} LICENSED_AS edges`);

  // =========================================================================
  // 10. SAME_ACCOUNT EDGES (businesses sharing an ACCOUNT NUMBER)
  // =========================================================================
  console.log("Extracting SAME_ACCOUNT edges...");
  const sameAcctBefore = edges.length;
  const multiSite = await query(
    conn,
    `SELECT CAST("ACCOUNT NUMBER" AS VARCHAR) as account_number,
            ARRAY_AGG(DISTINCT CAST("SITE NUMBER" AS VARCHAR)) as sites
     FROM city_businesses
     GROUP BY "ACCOUNT NUMBER"
     HAVING COUNT(DISTINCT "SITE NUMBER") > 1`
  );

  for (const r of multiSite) {
    const acct = r.account_number as string;
    const sites = r.sites as string[];
    // Create edges between all pairs (undirected, store both directions would be redundant)
    for (let i = 0; i < sites.length; i++) {
      for (let j = i + 1; j < sites.length; j++) {
        const srcId = `biz:${acct}:${sites[i]}`;
        const tgtId = `biz:${acct}:${sites[j]}`;
        if (!bizSeen.has(srcId) || !bizSeen.has(tgtId)) continue;
        edges.push({
          edge_id: `same_account:${acct}:${sites[i]}:${sites[j]}`,
          source_id: srcId,
          target_id: tgtId,
          relationship_type: "SAME_ACCOUNT",
          weight: 1.0,
        });
      }
    }
  }
  console.log(`  ${edges.length - sameAcctBefore} SAME_ACCOUNT edges`);

  // =========================================================================
  // 11. SAME_ENTITY EDGES (businesses sharing a LEGAL NAME across accounts)
  // =========================================================================
  console.log("Extracting SAME_ENTITY edges...");
  const sameEntBefore = edges.length;
  // Only create edges between DIFFERENT accounts sharing a legal name.
  // Limit to legal names with <= 50 locations to avoid combinatorial explosion on chains.
  const multiEntity = await query(
    conn,
    `WITH biz_keys AS (
       SELECT DISTINCT
         "LEGAL NAME" as legal_name,
         CAST("ACCOUNT NUMBER" AS VARCHAR) || ':' || CAST("SITE NUMBER" AS VARCHAR) as biz_key
       FROM city_businesses
       WHERE "LEGAL NAME" IS NOT NULL
     ),
     entity_groups AS (
       SELECT legal_name, ARRAY_AGG(biz_key) as biz_keys
       FROM biz_keys
       GROUP BY legal_name
       HAVING COUNT(DISTINCT biz_key) > 1 AND COUNT(DISTINCT biz_key) <= 50
     )
     SELECT legal_name, biz_keys FROM entity_groups`
  );

  for (const r of multiEntity) {
    const keys = r.biz_keys as string[];
    const legalName = r.legal_name as string;
    // Create a hub-and-spoke pattern: connect first to all others to limit edge count
    const hubId = `biz:${keys[0]}`;
    if (!bizSeen.has(hubId)) continue;
    for (let i = 1; i < keys.length; i++) {
      const spokeId = `biz:${keys[i]}`;
      if (!bizSeen.has(spokeId)) continue;
      edges.push({
        edge_id: `same_entity:${encodeURIComponent(legalName)}:${keys[0]}:${keys[i]}`,
        source_id: hubId,
        target_id: spokeId,
        relationship_type: "SAME_ENTITY",
        weight: 1.0,
      });
    }
  }
  console.log(`  ${edges.length - sameEntBefore} SAME_ENTITY edges`);

  // =========================================================================
  // 12. NEAR EDGES (businesses within threshold meters)
  // =========================================================================
  console.log(`Extracting NEAR edges (${NEAR_METERS}m threshold)...`);
  console.log("  This uses a spatial self-join — may take a moment...");
  const nearBefore = edges.length;

  // Use DuckDB spatial to compute distance-based pairs.
  // Deduplicate business locations first, then self-join.
  const nearPairs = await query(
    conn,
    `WITH biz_locs AS (
       SELECT DISTINCT
         CAST("ACCOUNT NUMBER" AS VARCHAR) || ':' || CAST("SITE NUMBER" AS VARCHAR) as biz_key,
         CAST("LATITUDE" AS DOUBLE) as lat,
         CAST("LONGITUDE" AS DOUBLE) as lng
       FROM city_businesses
       WHERE "LATITUDE" IS NOT NULL AND "LONGITUDE" IS NOT NULL
     )
     SELECT a.biz_key as a_key, b.biz_key as b_key,
            ST_Distance_Spheroid(
              ST_Point(a.lng, a.lat),
              ST_Point(b.lng, b.lat)
            ) as dist_m
     FROM biz_locs a
     JOIN biz_locs b ON a.biz_key < b.biz_key
       AND ABS(a.lat - b.lat) < 0.003   -- ~330m pre-filter on lat
       AND ABS(a.lng - b.lng) < 0.004   -- ~330m pre-filter on lng
     WHERE ST_Distance_Spheroid(
       ST_Point(a.lng, a.lat),
       ST_Point(b.lng, b.lat)
     ) <= ${NEAR_METERS}`
  );

  for (const r of nearPairs) {
    const srcId = `biz:${r.a_key}`;
    const tgtId = `biz:${r.b_key}`;
    if (!bizSeen.has(srcId) || !bizSeen.has(tgtId)) continue;
    const dist = r.dist_m as number;
    // Weight inversely proportional to distance: closer = higher weight
    const weight = Math.max(0, 1.0 - dist / NEAR_METERS);
    edges.push({
      edge_id: `near:${r.a_key}:${r.b_key}`,
      source_id: srcId,
      target_id: tgtId,
      relationship_type: "NEAR",
      weight: Math.round(weight * 1000) / 1000,
    });
  }
  console.log(`  ${edges.length - nearBefore} NEAR edges`);

  // =========================================================================
  // Write edges NDJSON
  // =========================================================================
  const edgesPath = join(OUT_DIR, "edges.ndjson");
  console.log(`\nWriting ${edges.length} edges to ${edgesPath}...`);
  await writeFile(edgesPath, ndjson(edges));

  // =========================================================================
  // Summary
  // =========================================================================
  console.log("\n=== Summary ===");
  console.log(`Nodes: ${nodes.length}`);
  console.log(`  Business:      ${bizSeen.size}`);
  console.log(`  Neighborhood:  ${neighborhoods.length}`);
  console.log(`  Ward:          ${wards.length}`);
  console.log(`  Activity:      ${activities.length}`);
  console.log(`  LicenseType:   ${licenseTypes.length}`);
  console.log(`Edges: ${edges.length}`);
  console.log(`Output: ${OUT_DIR}/`);

  await conn.close();
  await instance.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
