import React, { useState, useEffect, useRef, useCallback, useMemo } from 'react'
import { Loader2, ZoomIn, ZoomOut, Maximize2 } from 'lucide-react'
import ForceGraph2D from 'react-force-graph-2d'
import type { ForceGraphMethods, NodeObject } from 'react-force-graph-2d'
import { apiClient } from '../../../config/api'
import type { RagFilters } from './RagView'

interface GNode {
  id: string
  label: string
  type: string
  chunk_count: number
}

interface Edge {
  source: string
  target: string
  type: string
  strength: number
}

interface GraphData {
  nodes: GNode[]
  edges: Edge[]
  stats: {
    total_nodes: number
    total_edges: number
    entity_types: string[]
  }
}

type FGNode = NodeObject<GNode>

interface InteractiveGraphViewProps {
  filters: RagFilters
}

const TYPE_COLORS: Record<string, string> = {
  'PERSON': '#FF6B6B',
  'ORGANIZATION': '#4ECDC4',
  'CONCEPT': '#45B7D1',
  'METHOD': '#96CEB4',
  'TECHNOLOGY': '#FFEAA7',
  'LOCATION': '#DFE6E9',
  'METRIC': '#A29BFE',
  'WORK': '#FD79A8'
}

const DEFAULT_COLOR = '#95A5A6'

export const InteractiveGraphView: React.FC<InteractiveGraphViewProps> = ({ filters }) => {
  const [graphData, setGraphData] = useState<GraphData | null>(null)
  const [loading, setLoading] = useState(false)
  const [selectedNode, setSelectedNode] = useState<GNode | null>(null)
  const [dimensions, setDimensions] = useState<{ width: number; height: number } | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const fgRef = useRef<ForceGraphMethods<FGNode>>(undefined)

  useEffect(() => {
    fetchGraph()
  }, [filters])

  // Measure available space from viewport position
  useEffect(() => {
    const measure = () => {
      const el = containerRef.current
      if (!el) return
      const rect = el.getBoundingClientRect()
      const w = Math.floor(rect.width)
      const h = Math.floor(window.innerHeight - rect.top - 8)
      if (w > 0 && h > 0) {
        setDimensions(prev => {
          if (prev && prev.width === w && prev.height === h) return prev
          return { width: w, height: h }
        })
      }
    }

    measure()
    // Re-measure after a frame (layout may not be final yet)
    const raf = requestAnimationFrame(measure)
    window.addEventListener('resize', measure)
    return () => {
      cancelAnimationFrame(raf)
      window.removeEventListener('resize', measure)
    }
  }, [graphData])

  const fetchGraph = async () => {
    setLoading(true)
    try {
      const params: Record<string, string | number> = {
        min_strength: 0.1,
        limit: 500
      }

      if (filters.selectedDocuments.length > 0) {
        params.doc_ids = filters.selectedDocuments.join(',')
      }

      const response = await apiClient.get('/api/rag/graph', { params })
      setGraphData(response.data)
    } catch (error) {
      console.error('[InteractiveGraphView] Failed to fetch graph:', error)
      setGraphData({ nodes: [], edges: [], stats: { total_nodes: 0, total_edges: 0, entity_types: [] } })
    } finally {
      setLoading(false)
    }
  }

  // Configure d3 forces after graph data loads
  useEffect(() => {
    if (!fgRef.current || !graphData || graphData.nodes.length === 0) return

    const fg = fgRef.current
    const charge = fg.d3Force('charge')
    if (charge && typeof charge.strength === 'function') {
      charge.strength(-100)
    }
    const link = fg.d3Force('link')
    if (link && typeof link.distance === 'function') {
      link.distance((l: any) => 30 + (1 - (l.strength || 0.5)) * 60)
    }
    fg.d3ReheatSimulation()
  }, [graphData])

  // Convert API data to ForceGraph2D format
  const forceGraphData = useMemo(() => {
    if (!graphData) return { nodes: [], links: [] }
    return {
      nodes: graphData.nodes.map(n => ({ ...n })),
      links: graphData.edges.map(e => ({
        source: e.source,
        target: e.target,
        type: e.type,
        strength: e.strength
      }))
    }
  }, [graphData])

  // Configure forces after mount
  const handleEngineStop = useCallback(() => {
    // Fit graph into view after simulation settles
    if (fgRef.current && graphData && graphData.nodes.length > 0) {
      fgRef.current.zoomToFit(400, 40)
    }
  }, [graphData])

  // Custom node rendering
  const paintNode = useCallback((node: FGNode, ctx: CanvasRenderingContext2D, globalScale: number) => {
    const label = node.label || ''
    const type = node.type || ''
    const chunkCount = node.chunk_count || 0
    const radius = Math.log(chunkCount + 1) * 2 + 3
    const x = node.x ?? 0
    const y = node.y ?? 0
    const color = TYPE_COLORS[type] || DEFAULT_COLOR
    const isSelected = selectedNode?.id === node.id

    // Draw node circle
    ctx.beginPath()
    ctx.arc(x, y, radius, 0, 2 * Math.PI)
    ctx.fillStyle = color
    ctx.fill()

    // Highlight selected node
    if (isSelected) {
      ctx.strokeStyle = '#fff'
      ctx.lineWidth = 2 / globalScale
      ctx.stroke()
      ctx.beginPath()
      ctx.arc(x, y, radius + 2 / globalScale, 0, 2 * Math.PI)
      ctx.strokeStyle = color
      ctx.lineWidth = 1.5 / globalScale
      ctx.stroke()
    }

    // Draw labels when zoomed in enough or for large nodes
    const showLabel = globalScale > 1.5 || chunkCount > 5
    if (showLabel) {
      const fontSize = Math.max(10 / globalScale, 2)
      ctx.font = `${fontSize}px sans-serif`
      ctx.textAlign = 'center'
      ctx.textBaseline = 'top'
      // Dark text with white outline for readability on any background
      const text = label.slice(0, 20)
      const ty = y + radius + 2 / globalScale
      ctx.strokeStyle = 'rgba(255, 255, 255, 0.8)'
      ctx.lineWidth = 3 / globalScale
      ctx.strokeText(text, x, ty)
      ctx.fillStyle = '#1a1a2e'
      ctx.fillText(text, x, ty)
    }
  }, [selectedNode])

  // Hit area for nodes
  const paintNodeArea = useCallback((node: FGNode, color: string, ctx: CanvasRenderingContext2D) => {
    const chunkCount = node.chunk_count || 0
    const radius = Math.log(chunkCount + 1) * 2 + 3
    ctx.beginPath()
    ctx.arc(node.x ?? 0, node.y ?? 0, radius + 2, 0, 2 * Math.PI)
    ctx.fillStyle = color
    ctx.fill()
  }, [])

  const handleNodeClick = useCallback((node: FGNode) => {
    setSelectedNode({ id: node.id as string, label: node.label, type: node.type, chunk_count: node.chunk_count })
  }, [])

  const handleNodeDragEnd = useCallback((node: FGNode) => {
    // Pin node after drag
    node.fx = node.x
    node.fy = node.y
  }, [])

  const handleBackgroundClick = useCallback(() => {
    setSelectedNode(null)
  }, [])

  const handleZoomIn = useCallback(() => {
    if (fgRef.current) {
      const currentZoom = fgRef.current.zoom()
      fgRef.current.zoom(currentZoom * 1.5, 300)
    }
  }, [])

  const handleZoomOut = useCallback(() => {
    if (fgRef.current) {
      const currentZoom = fgRef.current.zoom()
      fgRef.current.zoom(currentZoom / 1.5, 300)
    }
  }, [])

  const handleZoomFit = useCallback(() => {
    if (fgRef.current) {
      fgRef.current.zoomToFit(400, 40)
    }
  }, [])

  if (loading) {
    return (
      <div className="flex justify-center items-center h-full">
        <Loader2 className="h-8 w-8 animate-spin text-primary" />
      </div>
    )
  }

  if (!graphData || graphData.nodes.length === 0) {
    return (
      <div className="text-center text-muted-foreground py-12">
        No graph data available. Try adjusting your filters or upload more documents.
      </div>
    )
  }

  return (
    <div className="flex" style={{ height: dimensions ? `${dimensions.height}px` : '100%' }}>
      {/* Graph */}
      <div className="flex-1 relative min-w-0" ref={containerRef}>
        <div className="absolute inset-0 rounded bg-card overflow-hidden">
          {dimensions && <ForceGraph2D
            ref={fgRef}
            graphData={forceGraphData}
            width={dimensions.width}
            height={dimensions.height}
            nodeCanvasObject={paintNode}
            nodeCanvasObjectMode={() => 'replace'}
            nodePointerAreaPaint={paintNodeArea}
            nodeLabel={(node: FGNode) => `${node.label} (${node.type}, ${node.chunk_count} chunks)`}
            linkWidth={(link: any) => Math.max(0.5, link.strength * 3)}
            linkColor={(link: any) => {
              const alpha = Math.min(1, 0.3 + link.strength * 0.5)
              return `rgba(150, 150, 150, ${alpha})`
            }}
            onNodeClick={handleNodeClick}
            onNodeDragEnd={handleNodeDragEnd}
            onBackgroundClick={handleBackgroundClick}
            onEngineStop={handleEngineStop}
            cooldownTicks={200}
            d3AlphaDecay={0.02}
            d3VelocityDecay={0.3}
            enableZoomInteraction={true}
            enablePanInteraction={true}
            enableNodeDrag={true}
            backgroundColor="transparent"
          />}
        </div>

        {/* Stats overlay */}
        <div className="absolute top-4 left-4 bg-card border border-border rounded p-2 text-xs text-muted-foreground pointer-events-none">
          {graphData.stats.total_nodes} nodes, {graphData.stats.total_edges} edges
        </div>

        {/* Legend overlay */}
        <div className="absolute top-4 right-4 bg-card border border-border rounded p-3 text-xs">
          <div className="font-medium mb-2">Legend</div>
          <div className="space-y-1">
            {graphData.stats.entity_types.map(type => (
              <div key={type} className="flex items-center gap-2">
                <div className="w-3 h-3 rounded-full" style={{ backgroundColor: TYPE_COLORS[type] || DEFAULT_COLOR }} />
                <span>{type}</span>
              </div>
            ))}
          </div>
        </div>

        {/* Zoom controls */}
        <div className="absolute bottom-4 right-4 flex flex-col gap-1">
          <button
            onClick={handleZoomIn}
            className="bg-card border border-border rounded p-1.5 hover:bg-accent text-muted-foreground hover:text-foreground transition-colors"
            title="Zoom in"
          >
            <ZoomIn className="h-4 w-4" />
          </button>
          <button
            onClick={handleZoomOut}
            className="bg-card border border-border rounded p-1.5 hover:bg-accent text-muted-foreground hover:text-foreground transition-colors"
            title="Zoom out"
          >
            <ZoomOut className="h-4 w-4" />
          </button>
          <button
            onClick={handleZoomFit}
            className="bg-card border border-border rounded p-1.5 hover:bg-accent text-muted-foreground hover:text-foreground transition-colors"
            title="Fit to view"
          >
            <Maximize2 className="h-4 w-4" />
          </button>
        </div>
      </div>

      {/* Selected Node Info */}
      {selectedNode && (
        <div className="w-64 border-l border-border bg-card p-4 space-y-3">
          <div>
            <div className="text-xs text-muted-foreground">Entity</div>
            <div className="font-medium">{selectedNode.label}</div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">Type</div>
            <div className="text-sm">{selectedNode.type}</div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">Occurrences</div>
            <div className="text-sm">{selectedNode.chunk_count} chunks</div>
          </div>
          <button
            onClick={() => setSelectedNode(null)}
            className="text-xs text-muted-foreground hover:text-foreground"
          >
            Close
          </button>
        </div>
      )}
    </div>
  )
}
