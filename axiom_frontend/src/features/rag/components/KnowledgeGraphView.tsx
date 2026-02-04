import React, { useState, useEffect } from 'react'
import { Loader2, Filter } from 'lucide-react'
import { apiClient } from '../../../config/api'
import type { RagFilters } from './RagView'

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

interface KnowledgeGraphViewProps {
  filters: RagFilters
}

export const KnowledgeGraphView: React.FC<KnowledgeGraphViewProps> = ({ filters }) => {
  const [graphData, setGraphData] = useState<GraphData | null>(null)
  const [selectedEntityTypes, setSelectedEntityTypes] = useState<string[]>([])
  const [loading, setLoading] = useState(false)
  const [showFilters, setShowFilters] = useState(false)

  const fetchGraph = async () => {
    setLoading(true)
    try {
      const params: any = {
        min_strength: 0.3,
        limit: 500
      }
      if (selectedEntityTypes.length > 0) params.entity_types = selectedEntityTypes
      if (filters.selectedDocuments.length > 0) params.doc_ids = filters.selectedDocuments.join(',')

      const response = await apiClient.get('/api/rag/graph', { params })
      setGraphData(response.data)
    } catch (error) {
      console.error('Failed to fetch graph:', error)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    fetchGraph()
  }, [selectedEntityTypes, filters])

  if (loading || !graphData) {
    return (
      <div className="flex justify-center items-center h-full">
        <Loader2 className="h-8 w-8 animate-spin text-primary" />
      </div>
    )
  }

  const nodesByType = graphData.nodes.reduce((acc, node) => {
    if (!acc[node.type]) acc[node.type] = []
    acc[node.type].push(node)
    return acc
  }, {} as Record<string, typeof graphData.nodes>)

  const toggleEntityType = (type: string) => {
    setSelectedEntityTypes(prev =>
      prev.includes(type) ? prev.filter(t => t !== type) : [...prev, type]
    )
  }

  return (
    <div className="space-y-6">
      {/* Header with Filters */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-4">
          <span className="inline-flex items-center px-3 py-1 rounded bg-primary/10 text-primary text-sm">
            Nodes: {graphData.stats.total_nodes}
          </span>
          <span className="inline-flex items-center px-3 py-1 rounded bg-primary/10 text-primary text-sm">
            Edges: {graphData.stats.total_edges}
          </span>
        </div>
        <button
          onClick={() => setShowFilters(!showFilters)}
          className="inline-flex items-center gap-2 px-3 py-2 border border-border rounded hover:bg-muted"
        >
          <Filter className="h-4 w-4" />
          Filters
        </button>
      </div>

      {/* Filter Panel */}
      {showFilters && (
        <div className="border border-border rounded-lg p-4">
          <div className="text-sm font-medium mb-2">Entity Types:</div>
          <div className="flex flex-wrap gap-2">
            {graphData.stats.entity_types.map((type) => (
              <button
                key={type}
                onClick={() => toggleEntityType(type)}
                className={`px-3 py-1 text-sm rounded border ${
                  selectedEntityTypes.includes(type)
                    ? 'bg-primary text-primary-foreground border-primary'
                    : 'bg-background border-border hover:bg-muted'
                }`}
              >
                {type}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Statistics Grid */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Entity Distribution */}
        <div className="border border-border rounded-lg p-4">
          <h3 className="text-lg font-semibold mb-4">Entity Distribution</h3>
          <div className="space-y-2">
            {Object.entries(nodesByType).map(([type, nodes]) => (
              <div key={type} className="flex items-center justify-between">
                <span className="text-sm">{type}</span>
                <span className="inline-flex items-center px-2 py-1 text-xs rounded bg-muted">
                  {nodes.length}
                </span>
              </div>
            ))}
          </div>
        </div>

        {/* Top Entities */}
        <div className="border border-border rounded-lg p-4">
          <h3 className="text-lg font-semibold mb-4">Top Entities</h3>
          <div className="space-y-2">
            {graphData.nodes
              .sort((a, b) => b.chunk_count - a.chunk_count)
              .slice(0, 10)
              .map((node) => (
                <div key={node.id} className="flex items-center justify-between">
                  <div>
                    <div className="text-sm">{node.label}</div>
                    <div className="text-xs text-muted-foreground">{node.type}</div>
                  </div>
                  <span className="inline-flex items-center px-2 py-1 text-xs rounded border border-border">
                    {node.chunk_count} chunks
                  </span>
                </div>
              ))}
          </div>
        </div>
      </div>

      {/* Entities by Type */}
      <div className="space-y-4">
        <h3 className="text-lg font-semibold">Entities by Type</h3>
        {Object.entries(nodesByType).map(([type, nodes]) => (
          <div key={type} className="border border-border rounded-lg p-4">
            <div className="text-sm font-medium mb-3">
              {type} ({nodes.length})
            </div>
            <div className="flex flex-wrap gap-2">
              {nodes.slice(0, 20).map((node) => (
                <span
                  key={node.id}
                  className="inline-flex items-center px-2 py-1 text-xs rounded border border-border hover:bg-muted"
                >
                  {node.label} ({node.chunk_count})
                </span>
              ))}
              {nodes.length > 20 && (
                <span className="inline-flex items-center px-2 py-1 text-xs rounded bg-primary/10 text-primary">
                  +{nodes.length - 20} more
                </span>
              )}
            </div>
          </div>
        ))}
      </div>

      {/* Visualization Note */}
      <div className="border border-blue-200 bg-blue-50 dark:bg-blue-950 dark:border-blue-800 rounded-lg p-4">
        <div className="text-sm">
          <strong>Note:</strong> Interactive graph visualization with force-directed layout
          can be added using libraries like react-force-graph or cytoscape.
          For now, entity statistics and lists are displayed above.
        </div>
      </div>
    </div>
  )
}
