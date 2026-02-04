import React, { useState } from 'react'
import { ChunksView } from './ChunksView'
import { KnowledgeGraphView } from './KnowledgeGraphView'
import { InteractiveGraphView } from './InteractiveGraphView'
import { RagSidebar } from './RagSidebar'

export interface RagFilters {
  selectedDocuments: string[]
  selectedGroups: string[]
  search: string
}

export const RagView: React.FC = () => {
  const [activeTab, setActiveTab] = useState<'chunks' | 'stats' | 'graph'>('chunks')
  const [filters, setFilters] = useState<RagFilters>({
    selectedDocuments: [],
    selectedGroups: [],
    search: ''
  })

  return (
    <div className="h-full flex bg-background">
      {/* Left Sidebar */}
      <RagSidebar filters={filters} onFiltersChange={setFilters} />

      {/* Main Content */}
      <div className="flex-1 flex flex-col">
        {/* Tabs */}
        <div className="border-b border-border bg-card">
          <div className="flex">
            <button
              onClick={() => setActiveTab('chunks')}
              className={`px-4 py-3 text-sm font-medium border-b-2 transition-colors ${
                activeTab === 'chunks'
                  ? 'border-primary text-primary'
                  : 'border-transparent text-muted-foreground hover:text-foreground'
              }`}
            >
              Chunks
            </button>
            <button
              onClick={() => setActiveTab('stats')}
              className={`px-4 py-3 text-sm font-medium border-b-2 transition-colors ${
                activeTab === 'stats'
                  ? 'border-primary text-primary'
                  : 'border-transparent text-muted-foreground hover:text-foreground'
              }`}
            >
              Statistics
            </button>
            <button
              onClick={() => setActiveTab('graph')}
              className={`px-4 py-3 text-sm font-medium border-b-2 transition-colors ${
                activeTab === 'graph'
                  ? 'border-primary text-primary'
                  : 'border-transparent text-muted-foreground hover:text-foreground'
              }`}
            >
              Interactive Graph
            </button>
          </div>
        </div>

        {/* Content */}
        <div className="flex-1 overflow-auto p-6">
          {activeTab === 'chunks' && <ChunksView filters={filters} />}
          {activeTab === 'stats' && <KnowledgeGraphView filters={filters} />}
          {activeTab === 'graph' && <InteractiveGraphView filters={filters} />}
        </div>
      </div>
    </div>
  )
}
