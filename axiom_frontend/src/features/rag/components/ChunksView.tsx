import React, { useState, useEffect } from 'react'
import { Search, Loader2, X, ExternalLink } from 'lucide-react'
import { apiClient } from '../../../config/api'
import type { RagFilters } from './RagView'

interface Chunk {
  chunk_id: string
  text: string
  metadata: any
  doc_id: string
  document_filename: string
  document_title: string
}

interface ChunkDetail extends Chunk {
  relationships: Array<{
    target_chunk_id: string
    type: string
    strength: number
    target_preview: string | null
  }>
  entities: Array<{
    text: string
    type: string
    occurrences: number
    relevance: number
  }>
}

interface ChunksViewProps {
  filters: RagFilters
}

export const ChunksView: React.FC<ChunksViewProps> = ({ filters }) => {
  const [chunks, setChunks] = useState<Chunk[]>([])
  const [page, setPage] = useState(1)
  const [totalPages, setTotalPages] = useState(1)
  const [loading, setLoading] = useState(false)
  const [selectedChunk, setSelectedChunk] = useState<ChunkDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  const fetchChunks = async () => {
    setLoading(true)
    try {
      const params: any = { page, limit: 50 }
      if (filters.search) params.search = filters.search
      if (filters.selectedDocuments.length > 0) params.doc_ids = filters.selectedDocuments.join(',')

      const response = await apiClient.get('/api/rag/chunks', { params })
      setChunks(response.data.chunks)
      setTotalPages(response.data.pagination.total_pages)
    } catch (error) {
      console.error('Failed to fetch chunks:', error)
    } finally {
      setLoading(false)
    }
  }

  const fetchChunkDetail = async (chunkId: string) => {
    setDetailLoading(true)
    try {
      const response = await apiClient.get(`/api/rag/chunks/${chunkId}`)
      setSelectedChunk(response.data)
    } catch (error) {
      console.error('Failed to fetch chunk detail:', error)
    } finally {
      setDetailLoading(false)
    }
  }

  useEffect(() => {
    fetchChunks()
  }, [page, filters])

  const handleChunkClick = (chunk: Chunk) => {
    fetchChunkDetail(chunk.chunk_id)
  }

  return (
    <div>
      {loading ? (
        <div className="flex justify-center items-center py-12">
          <Loader2 className="h-8 w-8 animate-spin text-primary" />
        </div>
      ) : (
        <>
          {/* Table */}
          <div className="border border-border rounded-lg overflow-hidden">
            <table className="w-full">
              <thead className="bg-muted">
                <tr>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase">Chunk ID</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase">Document</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase">Preview</th>
                  <th className="px-4 py-3 text-left text-xs font-medium text-muted-foreground uppercase">Metadata</th>
                </tr>
              </thead>
              <tbody className="bg-card divide-y divide-border">
                {chunks.map((chunk) => (
                  <tr
                    key={chunk.chunk_id}
                    onClick={() => handleChunkClick(chunk)}
                    className="hover:bg-muted/50 cursor-pointer transition-colors"
                  >
                    <td className="px-4 py-3">
                      <code className="text-xs text-muted-foreground">{chunk.chunk_id.slice(-12)}</code>
                    </td>
                    <td className="px-4 py-3">
                      <div className="text-sm">{chunk.document_title || chunk.document_filename}</div>
                    </td>
                    <td className="px-4 py-3">
                      <div className="text-sm text-muted-foreground truncate max-w-md">
                        {chunk.text}
                      </div>
                    </td>
                    <td className="px-4 py-3">
                      {chunk.metadata?.chunk_index !== undefined && (
                        <span className="inline-flex items-center px-2 py-1 text-xs rounded bg-primary/10 text-primary">
                          Index: {chunk.metadata.chunk_index}
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* Pagination */}
          <div className="flex justify-center items-center gap-2 mt-4">
            <button
              onClick={() => setPage(p => Math.max(1, p - 1))}
              disabled={page === 1}
              className="px-3 py-1 border border-border rounded disabled:opacity-50 disabled:cursor-not-allowed hover:bg-muted"
            >
              Previous
            </button>
            <span className="text-sm text-muted-foreground">
              Page {page} of {totalPages}
            </span>
            <button
              onClick={() => setPage(p => Math.min(totalPages, p + 1))}
              disabled={page === totalPages}
              className="px-3 py-1 border border-border rounded disabled:opacity-50 disabled:cursor-not-allowed hover:bg-muted"
            >
              Next
            </button>
          </div>
        </>
      )}

      {/* Chunk Detail Modal */}
      {selectedChunk && (
        <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
          <div className="bg-card border border-border rounded-lg max-w-3xl w-full max-h-[90vh] overflow-auto">
            {/* Header */}
            <div className="flex items-center justify-between p-4 border-b border-border sticky top-0 bg-card">
              <h2 className="text-lg font-semibold">Chunk Details</h2>
              <button
                onClick={() => setSelectedChunk(null)}
                className="p-1 hover:bg-muted rounded"
              >
                <X className="h-5 w-5" />
              </button>
            </div>

            {/* Content */}
            <div className="p-4 space-y-4">
              {detailLoading ? (
                <div className="flex justify-center py-12">
                  <Loader2 className="h-8 w-8 animate-spin text-primary" />
                </div>
              ) : (
                <>
                  <div>
                    <div className="text-sm font-medium text-muted-foreground mb-1">Chunk ID:</div>
                    <code className="text-xs bg-muted px-2 py-1 rounded">{selectedChunk.chunk_id}</code>
                  </div>

                  <div>
                    <div className="text-sm font-medium text-muted-foreground mb-1">Text:</div>
                    <div className="bg-muted/50 p-3 rounded max-h-60 overflow-auto">
                      <pre className="text-sm whitespace-pre-wrap">{selectedChunk.text}</pre>
                    </div>
                  </div>

                  <div>
                    <div className="text-sm font-medium text-muted-foreground mb-2">
                      Entities ({selectedChunk.entities.length}):
                    </div>
                    <div className="flex flex-wrap gap-2">
                      {selectedChunk.entities.map((entity, idx) => (
                        <span
                          key={idx}
                          className="inline-flex items-center px-2 py-1 text-xs rounded border border-border"
                        >
                          {entity.text} <span className="ml-1 text-muted-foreground">({entity.type})</span>
                        </span>
                      ))}
                    </div>
                  </div>

                  <div>
                    <div className="text-sm font-medium text-muted-foreground mb-2">
                      Relationships ({selectedChunk.relationships.length}):
                    </div>
                    <div className="border border-border rounded overflow-hidden">
                      <table className="w-full text-sm">
                        <thead className="bg-muted">
                          <tr>
                            <th className="px-3 py-2 text-left">Type</th>
                            <th className="px-3 py-2 text-left">Strength</th>
                            <th className="px-3 py-2 text-left">Target Chunk</th>
                          </tr>
                        </thead>
                        <tbody className="divide-y divide-border">
                          {selectedChunk.relationships.map((rel, idx) => (
                            <tr key={idx}>
                              <td className="px-3 py-2">{rel.type}</td>
                              <td className="px-3 py-2">{rel.strength.toFixed(2)}</td>
                              <td className="px-3 py-2">
                                <code className="text-xs">{rel.target_chunk_id.slice(-12)}</code>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </div>
                </>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
