import React, { useState } from 'react'
import { ChunksView } from './ChunksView'
import { KnowledgeGraphView } from './KnowledgeGraphView'
import { InteractiveGraphView } from './InteractiveGraphView'
import { DocumentGroupSidebar } from '../../documents/components/DocumentGroupSidebar'
import { useDocumentContext } from '../../documents/context/DocumentContext'
import { Library } from 'lucide-react'

export interface RagFilters {
  selectedDocuments: string[]
  selectedGroups: string[]
  search: string
}

export const RagView: React.FC = () => {
  const [activeTab, setActiveTab] = useState<'chunks' | 'stats' | 'graph'>('chunks')
  const { selectedGroup, setSelectedGroup } = useDocumentContext()

  // Convert DocumentContext state to RagFilters format
  const filters: RagFilters = {
    selectedDocuments: [],
    selectedGroups: selectedGroup ? [selectedGroup.id] : [],
    search: ''
  }

  const handleSelectGroup = (group: any) => {
    setSelectedGroup(group)
  }

  return (
    <div className="h-full flex bg-background">
      {/* Left Sidebar - reuses DocumentGroupSidebar for consistent layout */}
      <div className="w-60 border-r border-border bg-card flex-shrink-0 flex flex-col">
        <DocumentGroupSidebar onSelectGroup={handleSelectGroup} />
      </div>

      {/* Main Content */}
      <div className="flex-1 flex flex-col min-w-0">
        {/* Header matching Documents view */}
        <div className="border-b border-border bg-card px-6 py-4 flex items-center gap-2">
          <Library className="h-5 w-5 text-muted-foreground" />
          <h1 className="text-lg font-semibold">
            Knowledge Graph
            {selectedGroup && (
              <span className="text-muted-foreground font-normal"> / {selectedGroup.name}</span>
            )}
          </h1>
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
        <div className="flex-1 overflow-auto p-6">
          {activeTab === 'chunks' && <ChunksView filters={filters} />}
          {activeTab === 'stats' && <KnowledgeGraphView filters={filters} />}
          {activeTab === 'graph' && <InteractiveGraphView filters={filters} />}
        </div>
      </div>
    </div>
  )
}
