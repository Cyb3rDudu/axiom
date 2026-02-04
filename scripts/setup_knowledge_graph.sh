#!/bin/bash

# Knowledge Graph Setup Script
# This script sets up the knowledge graph layer for Axiom

set -e  # Exit on error

echo "==================================="
echo "Knowledge Graph Setup Script"
echo "==================================="
echo ""

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Check if running from project root
if [ ! -f "axiom_backend/main.py" ]; then
    echo -e "${RED}Error: Please run this script from the project root directory${NC}"
    exit 1
fi

echo "Step 1: Installing spaCy model..."
echo "=================================="
cd axiom_backend
if python -m spacy validate | grep -q "en_core_web_sm"; then
    echo -e "${GREEN}✓ spaCy model already installed${NC}"
else
    echo "Downloading spaCy model..."
    python -m spacy download en_core_web_sm
    echo -e "${GREEN}✓ spaCy model installed${NC}"
fi
cd ..
echo ""

echo "Step 2: Checking database connection..."
echo "========================================"

# Try to get DATABASE_URL from environment or .env
if [ -z "$DATABASE_URL" ]; then
    if [ -f ".env" ]; then
        export $(grep -v '^#' .env | grep DATABASE_URL | xargs)
    fi
fi

if [ -z "$DATABASE_URL" ]; then
    echo -e "${YELLOW}Warning: DATABASE_URL not set${NC}"
    echo "Please set DATABASE_URL or start Docker containers:"
    echo "  docker-compose up -d"
    echo ""
    echo "Then run: export DATABASE_URL='postgresql://user:pass@host:port/db'"
    exit 1
fi

echo "Testing database connection..."
if psql "$DATABASE_URL" -c "SELECT 1" > /dev/null 2>&1; then
    echo -e "${GREEN}✓ Database connection successful${NC}"
else
    echo -e "${YELLOW}Warning: Direct psql connection failed${NC}"
    echo "Trying Docker exec method..."

    # Extract container name from docker-compose or ps
    CONTAINER=$(docker ps --format '{{.Names}}' | grep postgres || echo "")

    if [ -z "$CONTAINER" ]; then
        echo -e "${RED}Error: Cannot connect to database${NC}"
        echo "Please ensure PostgreSQL is running:"
        echo "  docker-compose up -d postgres"
        exit 1
    fi

    echo "Found PostgreSQL container: $CONTAINER"
    USE_DOCKER_EXEC=true
fi
echo ""

echo "Step 3: Applying database migration..."
echo "======================================="

MIGRATION_FILE="axiom_backend/database/migrations/add_knowledge_graph_tables.sql"

if [ "$USE_DOCKER_EXEC" = true ]; then
    # Use Docker exec
    docker exec -i "$CONTAINER" psql -U axiom_user -d axiom_db < "$MIGRATION_FILE"
else
    # Use direct psql
    psql "$DATABASE_URL" -f "$MIGRATION_FILE"
fi

if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Migration applied successfully${NC}"
else
    echo -e "${RED}Error: Migration failed${NC}"
    exit 1
fi
echo ""

echo "Step 4: Verifying tables created..."
echo "===================================="

VERIFY_SQL="SELECT tablename FROM pg_tables WHERE tablename IN ('document_entities', 'entity_chunk_occurrences', 'chunk_relationships') ORDER BY tablename;"

if [ "$USE_DOCKER_EXEC" = true ]; then
    TABLES=$(docker exec "$CONTAINER" psql -U axiom_user -d axiom_db -t -c "$VERIFY_SQL")
else
    TABLES=$(psql "$DATABASE_URL" -t -c "$VERIFY_SQL")
fi

echo "Created tables:"
echo "$TABLES"

if echo "$TABLES" | grep -q "document_entities" && \
   echo "$TABLES" | grep -q "entity_chunk_occurrences" && \
   echo "$TABLES" | grep -q "chunk_relationships"; then
    echo -e "${GREEN}✓ All tables created successfully${NC}"
else
    echo -e "${YELLOW}Warning: Some tables may be missing${NC}"
fi
echo ""

echo "Step 5: Configuration recommendations..."
echo "========================================"
echo ""
echo "Add these to your .env file:"
echo ""
echo "# Knowledge Graph - Start Disabled"
echo "ENABLE_KNOWLEDGE_GRAPH=false"
echo "ENABLE_GRAPH_RETRIEVAL=false"
echo ""
echo "# Graph Retrieval Settings"
echo "GRAPH_MAX_DEPTH=2"
echo "GRAPH_MIN_STRENGTH=0.3"
echo "GRAPH_DECAY_FACTOR=0.6"
echo ""
echo "# Entity Extraction"
echo "ENTITY_ENABLE_LLM=false  # Start with spaCy only"
echo ""
echo -e "${YELLOW}Note: Keep graph features disabled initially for safe deployment${NC}"
echo ""

echo "==================================="
echo -e "${GREEN}Setup Complete!${NC}"
echo "==================================="
echo ""
echo "Next steps:"
echo "1. Add configuration to .env file (see above)"
echo "2. Restart your backend: docker-compose restart backend"
echo "3. Test with ENABLE_KNOWLEDGE_GRAPH=true"
echo "4. Monitor logs for 'Building knowledge graph...'"
echo ""
echo "See KNOWLEDGE_GRAPH_IMPLEMENTATION.md for full documentation"
