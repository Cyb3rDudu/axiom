-- Migration: Add document_images table for image embeddings
-- Date: 2026-02-03
-- Description: Adds support for storing image embeddings and metadata extracted from documents

-- Create document_images table
CREATE TABLE IF NOT EXISTS document_images (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    doc_id UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_id VARCHAR(255) REFERENCES document_chunks(chunk_id) ON DELETE CASCADE,
    image_id VARCHAR(255) UNIQUE NOT NULL,
    image_path TEXT NOT NULL,
    alt_text TEXT,
    image_embedding VECTOR(512),  -- CLIP ViT-B/32 produces 512-dimensional embeddings
    image_metadata JSONB DEFAULT '{}'::jsonb,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Create indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_document_images_doc_id ON document_images(doc_id);
CREATE INDEX IF NOT EXISTS idx_document_images_chunk_id ON document_images(chunk_id);
CREATE INDEX IF NOT EXISTS idx_document_images_image_id ON document_images(image_id);

-- Create HNSW index for vector similarity search on image embeddings
-- Using cosine distance for image similarity (standard for CLIP embeddings)
CREATE INDEX IF NOT EXISTS idx_document_images_embedding ON document_images
USING hnsw (image_embedding vector_cosine_ops)
WITH (m = 16, ef_construction = 64);

-- Add comment to table
COMMENT ON TABLE document_images IS 'Stores image embeddings and metadata for images extracted from documents using CLIP model';
COMMENT ON COLUMN document_images.image_embedding IS '512-dimensional CLIP ViT-B/32 embedding vector';
COMMENT ON COLUMN document_images.image_metadata IS 'Additional metadata about the image (e.g., dimensions, file size, position in document)';
