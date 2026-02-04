import React, { useState } from 'react'
import { ChunksView } from './ChunksView'
import { KnowledgeGraphView } from './KnowledgeGraphView'

export const RagView: React.FC = () => {
  const [activeTab, setActiveTab] = useState<'chunks' | 'graph'>('chunks')

  return (
    <div className="h-full flex flex-col bg-background">
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
            onClick={() => setActiveTab('graph')}
            className={`px-4 py-3 text-sm font-medium border-b-2 transition-colors ${
              activeTab === 'graph'
                ? 'border-primary text-primary'
                : 'border-transparent text-muted-foreground hover:text-foreground'
            }`}
          >
            Knowledge Graph
          </button>
        </div>
      </div>

      {/* Content */}
      <div className="flex-1 overflow-auto p-6">
        {activeTab === 'chunks' && <ChunksView />}
        {activeTab === 'graph' && <KnowledgeGraphView />}
      </div>
    </div>
  )
}
