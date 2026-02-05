import React, { useState, useEffect } from 'react'
import { Search, FileText, FolderOpen, ChevronDown, ChevronRight } from 'lucide-react'
import { apiClient } from '../../../config/api'
import type { RagFilters } from './RagView'

interface Document {
  id: string
  original_filename: string
  title: string
}

interface DocumentGroup {
  id: string
  name: string
}

interface RagSidebarProps {
  filters: RagFilters
  onFiltersChange: (filters: RagFilters) => void
}

export const RagSidebar: React.FC<RagSidebarProps> = ({ filters, onFiltersChange }) => {
  const [documents, setDocuments] = useState<Document[]>([])
  const [groups, setGroups] = useState<DocumentGroup[]>([])
  const [showDocuments, setShowDocuments] = useState(true)
  const [showGroups, setShowGroups] = useState(true)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    // Fetch documents and groups when component mounts
    fetchDocumentsAndGroups()
  }, [])

  const fetchDocumentsAndGroups = async () => {
    setLoading(true)
    try {
      console.log('[RagSidebar] Fetching documents and groups...')
      const [docsResponse, groupsResponse] = await Promise.all([
        apiClient.get('/api/documents', { params: { limit: 1000 } }),
        apiClient.get('/api/document-groups')
      ])
      console.log('[RagSidebar] Fetched', docsResponse.data.documents?.length || 0, 'documents and', groupsResponse.data.groups?.length || 0, 'groups')
      setDocuments(docsResponse.data.documents || [])
      setGroups(groupsResponse.data.groups || [])
    } catch (error) {
      console.error('[RagSidebar] Failed to fetch documents/groups:', error)
      // Set empty arrays on error to prevent undefined state
      setDocuments([])
      setGroups([])
    } finally {
      setLoading(false)
    }
  }

  const toggleDocument = (docId: string) => {
    const newSelected = filters.selectedDocuments.includes(docId)
      ? filters.selectedDocuments.filter(id => id !== docId)
      : [...filters.selectedDocuments, docId]
    onFiltersChange({ ...filters, selectedDocuments: newSelected })
  }

  const toggleGroup = (groupId: string) => {
    const newSelected = filters.selectedGroups.includes(groupId)
      ? filters.selectedGroups.filter(id => id !== groupId)
      : [...filters.selectedGroups, groupId]
    onFiltersChange({ ...filters, selectedGroups: newSelected })
  }

  const clearAll = () => {
    onFiltersChange({ selectedDocuments: [], selectedGroups: [], search: '' })
  }

  const hasFilters = filters.selectedDocuments.length > 0 || filters.selectedGroups.length > 0 || filters.search

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <div className="p-4 border-b border-border">
        <div className="flex items-center justify-between mb-3">
          <h3 className="font-semibold text-sm">Filters</h3>
          {hasFilters && (
            <button
              onClick={clearAll}
              className="text-xs text-muted-foreground hover:text-foreground"
            >
              Clear all
            </button>
          )}
        </div>

        {/* Search */}
        <div className="relative">
          <Search className="absolute left-2 top-1/2 transform -translate-y-1/2 h-3 w-3 text-muted-foreground" />
          <input
            type="text"
            placeholder="Search..."
            value={filters.search}
            onChange={(e) => onFiltersChange({ ...filters, search: e.target.value })}
            className="w-full pl-7 pr-2 py-1.5 text-xs border border-border rounded bg-background focus:outline-none focus:ring-1 focus:ring-primary"
          />
        </div>
      </div>

      {/* Content */}
      <div className="flex-1 overflow-auto p-2">
        {loading ? (
          <div className="text-xs text-muted-foreground text-center py-4">Loading...</div>
        ) : (
          <>
            {/* Document Groups */}
            <div className="mb-4">
              <button
                onClick={() => setShowGroups(!showGroups)}
                className="flex items-center gap-1 w-full px-2 py-1 hover:bg-muted rounded text-xs font-medium"
              >
                {showGroups ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                <FolderOpen className="h-3 w-3" />
                <span>Groups ({groups.length})</span>
              </button>

              {showGroups && (
                <div className="ml-4 mt-1 space-y-0.5">
                  {groups.map((group) => (
                    <label
                      key={group.id}
                      className="flex items-center gap-2 px-2 py-1 hover:bg-muted rounded cursor-pointer"
                    >
                      <input
                        type="checkbox"
                        checked={filters.selectedGroups.includes(group.id)}
                        onChange={() => toggleGroup(group.id)}
                        className="h-3 w-3"
                      />
                      <span className="text-xs truncate">{group.name}</span>
                    </label>
                  ))}
                </div>
              )}
            </div>

            {/* Documents */}
            <div>
              <button
                onClick={() => setShowDocuments(!showDocuments)}
                className="flex items-center gap-1 w-full px-2 py-1 hover:bg-muted rounded text-xs font-medium"
              >
                {showDocuments ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
                <FileText className="h-3 w-3" />
                <span>Documents ({documents.length})</span>
              </button>

              {showDocuments && (
                <div className="ml-4 mt-1 space-y-0.5 max-h-96 overflow-auto">
                  {documents.map((doc) => (
                    <label
                      key={doc.id}
                      className="flex items-center gap-2 px-2 py-1 hover:bg-muted rounded cursor-pointer"
                    >
                      <input
                        type="checkbox"
                        checked={filters.selectedDocuments.includes(doc.id)}
                        onChange={() => toggleDocument(doc.id)}
                        className="h-3 w-3"
                      />
                      <span className="text-xs truncate" title={doc.title || doc.original_filename}>
                        {doc.title || doc.original_filename}
                      </span>
                    </label>
                  ))}
                </div>
              )}
            </div>
          </>
        )}
      </div>

      {/* Footer - Selected Count */}
      {hasFilters && (
        <div className="p-3 border-t border-border text-xs text-muted-foreground">
          {filters.selectedDocuments.length > 0 && (
            <div>{filters.selectedDocuments.length} document(s) selected</div>
          )}
          {filters.selectedGroups.length > 0 && (
            <div>{filters.selectedGroups.length} group(s) selected</div>
          )}
        </div>
      )}
    </div>
  )
}
