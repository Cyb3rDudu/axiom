-- Knowledge Graph Layer Migration
-- Adds entity storage, relationships, and chunk connectivity for enhanced RAG retrieval

-- Entity storage
CREATE TABLE IF NOT EXISTS document_entities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_text VARCHAR(255) NOT NULL,
    entity_type VARCHAR(50) NOT NULL,  -- PERSON, ORGANIZATION, CONCEPT, METHOD, etc.
    canonical_form VARCHAR(255) NOT NULL,
    description TEXT,
    entity_metadata JSONB DEFAULT '{}',
    embedding vector(1024),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_entity_canonical ON document_entities(canonical_form, entity_type);
CREATE INDEX IF NOT EXISTS idx_entity_embedding ON document_entities USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS idx_entity_text_search ON document_entities USING gin(to_tsvector('english', entity_text || ' ' || COALESCE(description, '')));

-- Entity occurrences in chunks
CREATE TABLE IF NOT EXISTS entity_chunk_occurrences (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_id UUID NOT NULL REFERENCES document_entities(id) ON DELETE CASCADE,
    chunk_id VARCHAR(255) NOT NULL REFERENCES document_chunks(chunk_id) ON DELETE CASCADE,
    doc_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    occurrence_count INTEGER DEFAULT 1,
    context_snippet TEXT,
    position_in_chunk INTEGER,
    relevance_score FLOAT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(entity_id, chunk_id)
);

CREATE INDEX IF NOT EXISTS idx_occurrence_entity ON entity_chunk_occurrences(entity_id);
CREATE INDEX IF NOT EXISTS idx_occurrence_chunk ON entity_chunk_occurrences(chunk_id);
CREATE INDEX IF NOT EXISTS idx_occurrence_doc ON entity_chunk_occurrences(doc_id);

-- Entity relationships (graph edges)
CREATE TABLE IF NOT EXISTS entity_relationships (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_entity_id UUID NOT NULL REFERENCES document_entities(id) ON DELETE CASCADE,
    target_entity_id UUID NOT NULL REFERENCES document_entities(id) ON DELETE CASCADE,
    relationship_type VARCHAR(100) NOT NULL,  -- CITES, USES, EXTENDS, etc.
    relationship_strength FLOAT DEFAULT 0.5,
    evidence_count INTEGER DEFAULT 1,
    relationship_metadata JSONB DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    CHECK (source_entity_id != target_entity_id),
    UNIQUE(source_entity_id, target_entity_id, relationship_type)
);

CREATE INDEX IF NOT EXISTS idx_relationship_source ON entity_relationships(source_entity_id);
CREATE INDEX IF NOT EXISTS idx_relationship_target ON entity_relationships(target_entity_id);
CREATE INDEX IF NOT EXISTS idx_relationship_type ON entity_relationships(relationship_type);

-- Relationship evidence
CREATE TABLE IF NOT EXISTS relationship_evidence (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    relationship_id UUID NOT NULL REFERENCES entity_relationships(id) ON DELETE CASCADE,
    chunk_id VARCHAR(255) NOT NULL REFERENCES document_chunks(chunk_id) ON DELETE CASCADE,
    evidence_text TEXT,
    confidence_score FLOAT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(relationship_id, chunk_id)
);

CREATE INDEX IF NOT EXISTS idx_evidence_relationship ON relationship_evidence(relationship_id);
CREATE INDEX IF NOT EXISTS idx_evidence_chunk ON relationship_evidence(chunk_id);

-- Chunk relationships (simplified graph for retrieval)
CREATE TABLE IF NOT EXISTS chunk_relationships (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_chunk_id VARCHAR(255) NOT NULL REFERENCES document_chunks(chunk_id) ON DELETE CASCADE,
    target_chunk_id VARCHAR(255) NOT NULL REFERENCES document_chunks(chunk_id) ON DELETE CASCADE,
    relationship_type VARCHAR(50) NOT NULL,  -- 'sequential', 'citation', 'entity', 'section'
    strength FLOAT NOT NULL,  -- 0.0-1.0
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(source_chunk_id, target_chunk_id, relationship_type)
);

CREATE INDEX IF NOT EXISTS idx_chunk_rel_source ON chunk_relationships(source_chunk_id);
CREATE INDEX IF NOT EXISTS idx_chunk_rel_target ON chunk_relationships(target_chunk_id);
CREATE INDEX IF NOT EXISTS idx_chunk_rel_type ON chunk_relationships(relationship_type);
