# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Chicago Business Lead Generation and Analysis Platform that processes comprehensive business licensing data (58,108+ records) and provides both CLI tools and web interface for business intelligence and lead generation.

## Technology Stack

- **Backend**: Node.js with TypeScript
- **Database**: DuckDB (primary analytics), SQLite (embeddings)
- **Frontend**: Astro framework with MDX support
- **Data Processing**: LLM embeddings for semantic search, business clustering

## Essential Commands

### Database Operations
```bash
# Query the main business database
task query QUERY="SELECT * FROM city_businesses LIMIT 10"

# Show database schema
task schema

# Summarize table or query results
task summarize QUERY_OR_TABLE="city_businesses"
```

### Web Application (leads-web-preview/)
```bash
cd leads-web-preview/
npm run dev      # Start development server (localhost:4321)
npm run build    # Build for production
npm run preview  # Preview production build
```

### Data Processing
```bash
# Interactive business search using embeddings
./search.sh

# Neighborhood-based analysis
./use_by_hood.sh

# Generate site content from clusters
npx tsx make_site_docs.ts
```

## Key Databases

- **`chi-city-data.duckdb`** - Main Chicago business licensing data with geographic and activity information
- **`business_embeds.db`** - SQLite database containing business embeddings for semantic search
- **`leadgen.duckdb`** - Lead generation specific database
- **`business.db`** - Additional business data

## Architecture Overview

### Data Flow
1. **Raw Data**: Chicago business licensing CSV files
2. **Processing**: DuckDB analytics, clustering, LLM embeddings
3. **Storage**: Multiple specialized databases
4. **Access**: CLI tools for exploration, web interface for presentation

### Business Data Dimensions
- **Activity Types**: 366+ distinct business activities (food, retail, services, etc.)
- **Geographic**: 110+ neighborhoods, 50 wards, precise coordinates
- **Licensing**: 52+ license types with status and date tracking
- **Scale**: 58,108+ active business records

### Web Application Structure
- **Framework**: Astro with TypeScript (strict mode)
- **Content**: Auto-generated MDX pages from business clusters
- **Search**: Pagefind integration
- **Performance**: Optimized for 100/100 Lighthouse scores

## Development Patterns

### Database-First Approach
- DuckDB for complex analytical queries
- SQL-first data exploration
- Async/await patterns for database operations

### Content Generation
- Automated markdown creation from JSON clusters
- TypeScript scripts generate Astro content pages
- Business groups organized by similarity clusters

### Interactive CLI Tools
- Uses external tools: fzf, gum, bat, jq
- Shell scripts for data exploration workflows
- Real-time database querying and filtering

## Key Business Query Patterns

```sql
-- Find businesses by activity type
SELECT * FROM city_businesses WHERE activity LIKE '%Food%';

-- Geographic clustering
SELECT PRI_NEIGH, COUNT(*) FROM city_businesses GROUP BY PRI_NEIGH;

-- License status monitoring
SELECT * FROM city_businesses WHERE "LICENSE TERM EXPIRATION DATE" > CURRENT_DATE;
```

## External Dependencies

- **Foursquare API**: Requires `FOURSQUARE_API_KEY` environment variable
- **CLI Tools**: fzf, gum, bat, jq (for interactive scripts)
- **Node.js**: @duckdb/node-api, duckdb-async packages

## File Organization

- **Root**: Database files (.db, .duckdb), raw data (.csv), processing scripts (.ts, .sh)
- **leads-web-preview/**: Astro web application
- **content/**: Additional content and auto-generated business group pages
- **Taskfile.agent_mcp.yaml**: Database operation definitions