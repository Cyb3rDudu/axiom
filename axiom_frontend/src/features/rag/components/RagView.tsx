import React, { useState } from 'react'
import { Box, Tabs, Tab } from '@mui/material'
import { ChunksView } from './ChunksView'
import { KnowledgeGraphView } from './KnowledgeGraphView'

export const RagView: React.FC = () => {
  const [activeTab, setActiveTab] = useState(0)

  return (
    <Box sx={{ width: '100%', height: '100%', display: 'flex', flexDirection: 'column', bgcolor: 'background.default' }}>
      <Box sx={{ borderBottom: 1, borderColor: 'divider', bgcolor: 'background.paper' }}>
        <Tabs value={activeTab} onChange={(_, value) => setActiveTab(value)}>
          <Tab label="Chunks" />
          <Tab label="Knowledge Graph" />
        </Tabs>
      </Box>

      <Box sx={{ flex: 1, overflow: 'auto', p: 3 }}>
        {activeTab === 0 && <ChunksView />}
        {activeTab === 1 && <KnowledgeGraphView />}
      </Box>
    </Box>
  )
}
