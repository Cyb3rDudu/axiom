import React, { useState } from 'react'
import { ChunksView } from './ChunksView'
import { KnowledgeGraphView } from './KnowledgeGraphView'
import { InteractiveGraphView } from './InteractiveGraphView'
import { useDocumentContext } from '../../documents/context/DocumentContext'
import { Library } from 'lucide-react'

export interface RagFilters {
  selectedDocuments: string[]
  selectedGroups: string[]
  search: string
}

export const RagView: React.FC = () => {
  const [activeTab, setActiveTab] = useState<'chunks' | 'stats' | 'graph'>('chunks')
  const { selectedGroup } = useDocumentContext()

  // Convert DocumentContext state to RagFilters format
  const filters: RagFilters = {
    selectedDocuments: [],
    selectedGroups: selectedGroup ? [selectedGroup.id] : [],
    search: ''
  }

  return (
    <div className="h-full flex flex-col bg-background">
      {/* Header - EXACTLY matching Documents view */}
      <div className="px-6 py-4 border-b border-border min-h-[88px] flex items-center bg-header-background">
        <div className="flex items-center space-x-2">
          <Library className="h-5 w-5 text-primary" />
          <span className="text-lg font-semibold text-foreground">Knowledge Graph</span>
          <span className="text-muted-foreground">/</span>
          <span className="text-lg font-medium text-foreground">
            {selectedGroup ? selectedGroup.name : 'All Documents'}
          </span>
        </div>
      </div>

      {/* Tabs */}
      <div className="border-b border-border bg-card">
        <div className="flex px-6">
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
      <div className={`flex-1 min-h-0 ${activeTab === 'graph' ? 'overflow-hidden p-2' : 'overflow-auto p-6'}`}>
        {activeTab === 'chunks' && <ChunksView filters={filters} />}
        {activeTab === 'stats' && <KnowledgeGraphView filters={filters} />}
        {activeTab === 'graph' && <InteractiveGraphView filters={filters} />}
      </div>
    </div>
  )
}
