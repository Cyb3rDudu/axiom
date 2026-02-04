import React, { useState, useEffect } from 'react'
import {
  Table, TableBody, TableCell, TableContainer, TableHead, TableRow,
  Paper, TextField, Pagination, Chip, Box, Typography, CircularProgress,
  Dialog, DialogTitle, DialogContent, IconButton
} from '@mui/material'
import { Close as CloseIcon } from '@mui/icons-material'
import { apiClient } from '../../../config/api'

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

export const ChunksView: React.FC = () => {
  const [chunks, setChunks] = useState<Chunk[]>([])
  const [page, setPage] = useState(1)
  const [totalPages, setTotalPages] = useState(1)
  const [search, setSearch] = useState('')
  const [loading, setLoading] = useState(false)
  const [selectedChunk, setSelectedChunk] = useState<ChunkDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  const fetchChunks = async () => {
    setLoading(true)
    try {
      const response = await apiClient.get('/api/rag/chunks', {
        params: { page, limit: 50, search: search || undefined }
      })
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
  }, [page, search])

  const handleChunkClick = (chunk: Chunk) => {
    fetchChunkDetail(chunk.chunk_id)
  }

  return (
    <Box>
      <TextField
        fullWidth
        placeholder="Search chunks..."
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        sx={{ mb: 2 }}
      />

      {loading ? (
        <Box display="flex" justifyContent="center" p={4}>
          <CircularProgress />
        </Box>
      ) : (
        <>
          <TableContainer component={Paper}>
            <Table>
              <TableHead>
                <TableRow>
                  <TableCell>Chunk ID</TableCell>
                  <TableCell>Document</TableCell>
                  <TableCell>Preview</TableCell>
                  <TableCell>Metadata</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {chunks.map((chunk) => (
                  <TableRow
                    key={chunk.chunk_id}
                    hover
                    onClick={() => handleChunkClick(chunk)}
                    sx={{ cursor: 'pointer' }}
                  >
                    <TableCell>
                      <Typography variant="body2" sx={{ fontFamily: 'monospace', fontSize: '0.75rem' }}>
                        {chunk.chunk_id.slice(-12)}
                      </Typography>
                    </TableCell>
                    <TableCell>
                      <Typography variant="body2">{chunk.document_title || chunk.document_filename}</Typography>
                    </TableCell>
                    <TableCell>
                      <Typography
                        variant="body2"
                        sx={{
                          maxWidth: 400,
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          whiteSpace: 'nowrap'
                        }}
                      >
                        {chunk.text}
                      </Typography>
                    </TableCell>
                    <TableCell>
                      {chunk.metadata?.chunk_index !== undefined && (
                        <Chip label={`Index: ${chunk.metadata.chunk_index}`} size="small" />
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>

          <Box sx={{ display: 'flex', justifyContent: 'center', mt: 2 }}>
            <Pagination
              count={totalPages}
              page={page}
              onChange={(_, value) => setPage(value)}
            />
          </Box>
        </>
      )}

      {/* Chunk Detail Dialog */}
      <Dialog
        open={!!selectedChunk}
        onClose={() => setSelectedChunk(null)}
        maxWidth="md"
        fullWidth
      >
        {selectedChunk && (
          <>
            <DialogTitle>
              Chunk Details
              <IconButton
                onClick={() => setSelectedChunk(null)}
                sx={{ position: 'absolute', right: 8, top: 8 }}
              >
                <CloseIcon />
              </IconButton>
            </DialogTitle>
            <DialogContent dividers>
              {detailLoading ? (
                <Box display="flex" justifyContent="center" p={4}>
                  <CircularProgress />
                </Box>
              ) : (
                <>
                  <Typography variant="subtitle2" gutterBottom>Chunk ID:</Typography>
                  <Typography variant="body2" sx={{ fontFamily: 'monospace', mb: 2 }}>
                    {selectedChunk.chunk_id}
                  </Typography>

                  <Typography variant="subtitle2" gutterBottom>Text:</Typography>
                  <Paper sx={{ p: 2, mb: 2, bgcolor: 'grey.50', maxHeight: 300, overflow: 'auto' }}>
                    <Typography variant="body2" sx={{ whiteSpace: 'pre-wrap' }}>
                      {selectedChunk.text}
                    </Typography>
                  </Paper>

                  <Typography variant="subtitle2" gutterBottom>Entities ({selectedChunk.entities.length}):</Typography>
                  <Box sx={{ mb: 2, display: 'flex', flexWrap: 'wrap', gap: 1 }}>
                    {selectedChunk.entities.map((entity, idx) => (
                      <Chip
                        key={idx}
                        label={`${entity.text} (${entity.type})`}
                        size="small"
                        variant="outlined"
                      />
                    ))}
                  </Box>

                  <Typography variant="subtitle2" gutterBottom>
                    Relationships ({selectedChunk.relationships.length}):
                  </Typography>
                  <TableContainer component={Paper} variant="outlined">
                    <Table size="small">
                      <TableHead>
                        <TableRow>
                          <TableCell>Type</TableCell>
                          <TableCell>Strength</TableCell>
                          <TableCell>Target Chunk</TableCell>
                        </TableRow>
                      </TableHead>
                      <TableBody>
                        {selectedChunk.relationships.map((rel, idx) => (
                          <TableRow key={idx}>
                            <TableCell>{rel.type}</TableCell>
                            <TableCell>{rel.strength.toFixed(2)}</TableCell>
                            <TableCell sx={{ fontFamily: 'monospace', fontSize: '0.75rem' }}>
                              {rel.target_chunk_id.slice(-12)}
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </TableContainer>
                </>
              )}
            </DialogContent>
          </>
        )}
      </Dialog>
    </Box>
  )
}
