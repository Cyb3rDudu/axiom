import React, { useState, useEffect, useRef } from 'react'
import { Loader2 } from 'lucide-react'
import { apiClient } from '../../../config/api'
import type { RagFilters } from './RagView'

interface Node {
  id: string
  label: string
  type: string
  chunk_count: number
  x?: number
  y?: number
  vx?: number
  vy?: number
}

interface Edge {
  source: string
  target: string
  type: string
  strength: number
}

interface GraphData {
  nodes: Node[]
  edges: Edge[]
  stats: {
    total_nodes: number
    total_edges: number
    entity_types: string[]
  }
}

interface InteractiveGraphViewProps {
  filters: RagFilters
}

export const InteractiveGraphView: React.FC<InteractiveGraphViewProps> = ({ filters }) => {
  const [graphData, setGraphData] = useState<GraphData | null>(null)
  const [loading, setLoading] = useState(false)
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const animFrameRef = useRef<number>(0)
  const [selectedNode, setSelectedNode] = useState<Node | null>(null)

  useEffect(() => {
    fetchGraph()
  }, [filters])

  const fetchGraph = async () => {
    setLoading(true)
    try {
      const params: any = {
        min_strength: 0.1,
        limit: 200
      }

      // Apply filters if any are selected
      if (filters.selectedDocuments.length > 0) {
        params.doc_ids = filters.selectedDocuments.join(',')
      }

      const response = await apiClient.get('/api/rag/graph', { params })
      const data = response.data

      // Initialize node positions randomly
      const nodes = (data.nodes || []).map((node: Node) => ({
        ...node,
        x: Math.random() * 800,
        y: Math.random() * 600,
        vx: 0,
        vy: 0
      }))

      setGraphData({ ...data, nodes })
    } catch (error) {
      console.error('[InteractiveGraphView] Failed to fetch graph:', error)
      setGraphData({ nodes: [], edges: [], stats: { total_nodes: 0, total_edges: 0, entity_types: [] } })
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (!graphData || !canvasRef.current) return

    const canvas = canvasRef.current
    const ctx = canvas.getContext('2d')
    if (!ctx) return

    // Set canvas size
    canvas.width = canvas.offsetWidth
    canvas.height = canvas.offsetHeight

    const typeColors: Record<string, string> = {
      'PERSON': '#FF6B6B',
      'ORGANIZATION': '#4ECDC4',
      'CONCEPT': '#45B7D1',
      'METHOD': '#96CEB4',
      'TECHNOLOGY': '#FFEAA7',
      'LOCATION': '#DFE6E9',
      'METRIC': '#A29BFE',
      'WORK': '#FD79A8'
    }

    let frameCount = 0

    // Simple force simulation
    const simulate = () => {
      const nodes = graphData.nodes
      const edges = graphData.edges
      frameCount++

      // Increase damping after 300 frames to converge
      const damping = frameCount > 300 ? 0.6 : 0.85
      const forceScale = frameCount > 300 ? 0.005 : 0.02

      // Apply forces
      for (let i = 0; i < nodes.length; i++) {
        let fx = 0, fy = 0

        // Repulsion between nodes
        for (let j = 0; j < nodes.length; j++) {
          if (i === j) continue
          const dx = nodes[i].x! - nodes[j].x!
          const dy = nodes[i].y! - nodes[j].y!
          const dist = Math.sqrt(dx * dx + dy * dy) || 1
          const force = 500 / (dist * dist)
          fx += (dx / dist) * force
          fy += (dy / dist) * force
        }

        // Attraction along edges
        edges.forEach(edge => {
          const sourceIdx = nodes.findIndex(n => n.id === edge.source)
          const targetIdx = nodes.findIndex(n => n.id === edge.target)
          if (sourceIdx < 0 || targetIdx < 0) return
          if (sourceIdx === i || targetIdx === i) {
            const other = sourceIdx === i ? nodes[targetIdx] : nodes[sourceIdx]
            const dx = other.x! - nodes[i].x!
            const dy = other.y! - nodes[i].y!
            const dist = Math.sqrt(dx * dx + dy * dy) || 1
            const force = dist * 0.03 * edge.strength
            fx += (dx / dist) * force
            fy += (dy / dist) * force
          }
        })

        // Center gravity
        const centerX = canvas.width / 2
        const centerY = canvas.height / 2
        fx += (centerX - nodes[i].x!) * 0.01
        fy += (centerY - nodes[i].y!) * 0.01

        nodes[i].vx = (nodes[i].vx || 0) * damping + fx * forceScale
        nodes[i].vy = (nodes[i].vy || 0) * damping + fy * forceScale
        nodes[i].x! += nodes[i].vx || 0
        nodes[i].y! += nodes[i].vy || 0

        // Keep in bounds
        nodes[i].x = Math.max(20, Math.min(canvas.width - 20, nodes[i].x!))
        nodes[i].y = Math.max(20, Math.min(canvas.height - 20, nodes[i].y!))
      }

      // Render
      ctx.clearRect(0, 0, canvas.width, canvas.height)

      // Draw edges
      edges.forEach(edge => {
        const source = nodes.find(n => n.id === edge.source)
        const target = nodes.find(n => n.id === edge.target)
        if (source && target) {
          const alpha = Math.min(1, 0.4 + edge.strength * 0.4)
          ctx.strokeStyle = `rgba(100, 100, 100, ${alpha})`
          ctx.lineWidth = Math.max(1, edge.strength * 3)
          ctx.beginPath()
          ctx.moveTo(source.x!, source.y!)
          ctx.lineTo(target.x!, target.y!)
          ctx.stroke()
        }
      })

      // Draw nodes
      nodes.forEach(node => {
        const radius = Math.log(node.chunk_count + 1) * 3 + 4
        ctx.fillStyle = typeColors[node.type] || '#95A5A6'
        ctx.beginPath()
        ctx.arc(node.x!, node.y!, radius, 0, 2 * Math.PI)
        ctx.fill()

        // Draw label for larger nodes
        if (node.chunk_count > 3) {
          ctx.fillStyle = '#fff'
          ctx.font = '10px sans-serif'
          ctx.textAlign = 'center'
          ctx.fillText(node.label.slice(0, 15), node.x!, node.y! + radius + 12)
        }
      })

      animFrameRef.current = requestAnimationFrame(simulate)
    }

    simulate()

    return () => {
      cancelAnimationFrame(animFrameRef.current)
    }
  }, [graphData])

  const handleCanvasClick = (e: React.MouseEvent<HTMLCanvasElement>) => {
    if (!graphData || !canvasRef.current) return

    const rect = canvasRef.current.getBoundingClientRect()
    const x = e.clientX - rect.left
    const y = e.clientY - rect.top

    const clicked = graphData.nodes.find(node => {
      const dx = node.x! - x
      const dy = node.y! - y
      const radius = Math.log(node.chunk_count + 1) * 3 + 4
      return Math.sqrt(dx * dx + dy * dy) < radius
    })

    setSelectedNode(clicked || null)
  }

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
    <div className="h-full flex">
      {/* Canvas */}
      <div className="flex-1 relative">
        <canvas
          ref={canvasRef}
          onClick={handleCanvasClick}
          className="w-full h-full border border-border rounded bg-card cursor-pointer"
          style={{ minHeight: '600px' }}
        />
        <div className="absolute top-4 left-4 bg-card border border-border rounded p-2 text-xs text-muted-foreground">
          {graphData.stats.total_nodes} nodes, {graphData.stats.total_edges} edges
        </div>
        <div className="absolute top-4 right-4 bg-card border border-border rounded p-3 text-xs">
          <div className="font-medium mb-2">Legend</div>
          <div className="space-y-1">
            {graphData.stats.entity_types.map(type => (
              <div key={type} className="flex items-center gap-2">
                <div className="w-3 h-3 rounded-full" style={{
                  backgroundColor: {
                    'PERSON': '#FF6B6B',
                    'ORGANIZATION': '#4ECDC4',
                    'CONCEPT': '#45B7D1',
                    'METHOD': '#96CEB4',
                    'TECHNOLOGY': '#FFEAA7',
                    'LOCATION': '#DFE6E9',
                    'METRIC': '#A29BFE',
                    'WORK': '#FD79A8'
                  }[type] || '#95A5A6'
                }} />
                <span>{type}</span>
              </div>
            ))}
          </div>
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
