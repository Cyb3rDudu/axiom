import React, { useState, useEffect } from 'react'
import {
  Box, FormControl, InputLabel, Select, MenuItem, Chip, CircularProgress,
  Typography, Paper, Grid, Card, CardContent
} from '@mui/material'
import { apiClient } from '../../../config/api'

interface GraphData {
  nodes: Array<{
    id: string
    label: string
    type: string
    chunk_count: number
    doc_count: number
  }>
  edges: Array<{
    source: string
    target: string
    type: string
    strength: number
    evidence_count: number
  }>
  stats: {
    total_nodes: number
    total_edges: number
    entity_types: string[]
  }
}

export const KnowledgeGraphView: React.FC = () => {
  const [graphData, setGraphData] = useState<GraphData | null>(null)
  const [selectedEntityTypes, setSelectedEntityTypes] = useState<string[]>([])
  const [loading, setLoading] = useState(false)

  const fetchGraph = async () => {
    setLoading(true)
    try {
      const response = await apiClient.get('/api/rag/graph', {
        params: {
          entity_types: selectedEntityTypes.length > 0 ? selectedEntityTypes : undefined,
          min_strength: 0.3,
          limit: 500
        }
      })
      setGraphData(response.data)
    } catch (error) {
      console.error('Failed to fetch graph:', error)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    fetchGraph()
  }, [selectedEntityTypes])

  if (loading || !graphData) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '100%' }}>
        <CircularProgress />
      </Box>
    )
  }

  // Group nodes by type for display
  const nodesByType = graphData.nodes.reduce((acc, node) => {
    if (!acc[node.type]) acc[node.type] = []
    acc[node.type].push(node)
    return acc
  }, {} as Record<string, typeof graphData.nodes>)

  return (
    <Box sx={{ width: '100%', height: '100%' }}>
      <Box sx={{ mb: 3 }}>
        <FormControl size="small" sx={{ minWidth: 300 }}>
          <InputLabel>Filter Entity Types</InputLabel>
          <Select
            multiple
            value={selectedEntityTypes}
            onChange={(e) => setSelectedEntityTypes(e.target.value as string[])}
            renderValue={(selected) => (
              <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}>
                {selected.map((value) => (
                  <Chip key={value} label={value} size="small" />
                ))}
              </Box>
            )}
          >
            {graphData.stats.entity_types.map((type) => (
              <MenuItem key={type} value={type}>{type}</MenuItem>
            ))}
          </Select>
        </FormControl>

        <Box sx={{ mt: 2, display: 'flex', gap: 2 }}>
          <Chip label={`Nodes: ${graphData.stats.total_nodes}`} />
          <Chip label={`Edges: ${graphData.stats.total_edges}`} />
        </Box>
      </Box>

      {/* Statistics Cards */}
      <Grid container spacing={2} sx={{ mb: 3 }}>
        <Grid item xs={12} md={6}>
          <Card>
            <CardContent>
              <Typography variant="h6" gutterBottom>Entity Distribution</Typography>
              {Object.entries(nodesByType).map(([type, nodes]) => (
                <Box key={type} sx={{ mb: 1, display: 'flex', justifyContent: 'space-between' }}>
                  <Typography variant="body2">{type}</Typography>
                  <Chip label={nodes.length} size="small" />
                </Box>
              ))}
            </CardContent>
          </Card>
        </Grid>

        <Grid item xs={12} md={6}>
          <Card>
            <CardContent>
              <Typography variant="h6" gutterBottom>Top Entities</Typography>
              {graphData.nodes
                .sort((a, b) => b.chunk_count - a.chunk_count)
                .slice(0, 10)
                .map((node) => (
                  <Box key={node.id} sx={{ mb: 1, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <Box>
                      <Typography variant="body2">{node.label}</Typography>
                      <Typography variant="caption" color="text.secondary">{node.type}</Typography>
                    </Box>
                    <Chip label={`${node.chunk_count} chunks`} size="small" variant="outlined" />
                  </Box>
                ))}
            </CardContent>
          </Card>
        </Grid>
      </Grid>

      {/* Entity List by Type */}
      <Typography variant="h6" gutterBottom sx={{ mt: 3 }}>Entities by Type</Typography>
      {Object.entries(nodesByType).map(([type, nodes]) => (
        <Paper key={type} sx={{ p: 2, mb: 2 }}>
          <Typography variant="subtitle1" gutterBottom>{type} ({nodes.length})</Typography>
          <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1 }}>
            {nodes.slice(0, 20).map((node) => (
              <Chip
                key={node.id}
                label={`${node.label} (${node.chunk_count})`}
                size="small"
                variant="outlined"
              />
            ))}
            {nodes.length > 20 && (
              <Chip label={`+${nodes.length - 20} more`} size="small" variant="outlined" color="primary" />
            )}
          </Box>
        </Paper>
      ))}

      {/* Visualization Note */}
      <Paper sx={{ p: 3, mt: 3, bgcolor: 'info.light', color: 'info.contrastText' }}>
        <Typography variant="body2">
          <strong>Note:</strong> Interactive graph visualization with force-directed layout
          can be added using libraries like react-force-graph or cytoscape.
          For now, entity statistics and lists are displayed above.
        </Typography>
      </Paper>
    </Box>
  )
}
