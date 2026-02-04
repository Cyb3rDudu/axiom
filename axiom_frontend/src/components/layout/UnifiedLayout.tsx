import React from 'react'
import { useViewStore } from '../../stores/viewStore'
import { ChatView } from '../../features/chat/components/ChatView'
import { WritingView } from '../../features/writing/components/WritingView'
import { RagView } from '../../features/rag/components/RagView'
import DocumentsPage from '../../pages/DocumentsPage'

export const UnifiedLayout: React.FC = () => {
  const { currentView } = useViewStore()

  return (
    <div className="h-full">
      {currentView === 'research' ? (
        <ChatView />
      ) : currentView === 'writing' ? (
        <WritingView />
      ) : currentView === 'rag' ? (
        <RagView />
      ) : (
        <DocumentsPage />
      )}
    </div>
  )
}
