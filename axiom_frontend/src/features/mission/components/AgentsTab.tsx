import React, { useState, useEffect, useMemo } from 'react';
import { useMissionStore } from '../store';
import { 
  AgentActivityLog, 
  MissionStatsDashboard, 
  AgentStatusIndicator,
} from '../../../components/mission';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '../../../components/ui/tabs';

interface AgentsTabProps {
  missionId: string;
  hasMoreLogs?: boolean;
  onLoadMoreLogs?: () => void;
  onLoadAllLogs?: () => void;
  isLoadingMoreLogs?: boolean;
  totalLogsCount?: number;
}

export const AgentsTab: React.FC<AgentsTabProps> = ({
  missionId,
  hasMoreLogs,
  onLoadMoreLogs,
  onLoadAllLogs,
  isLoadingMoreLogs,
  totalLogsCount
}) => {
  const { activeMission, missionLogs, fetchMissionLogs } = useMissionStore();
  const [isLoading, setIsLoading] = useState(false);

  // Get logs from the store (shared with ResearchPanel)
  const logs = missionLogs[missionId] || [];

  // Memoize logs to prevent unnecessary re-renders
  const memoizedLogs = useMemo(() => {
    return logs;
  }, [logs]);

  // Catch-up fetch: when this tab mounts with an active mission but empty logs,
  // fetch from the API to recover logs that may have been missed by WebSocket.
  // This handles the case where the user navigates away and back while mission runs.
  useEffect(() => {
    if (missionId && logs.length === 0 && activeMission?.status === 'running') {
      setIsLoading(true);
      fetchMissionLogs(missionId).finally(() => setIsLoading(false));
    } else if (missionId && logs.length === 0) {
      setIsLoading(true);
      const timer = setTimeout(() => setIsLoading(false), 1000);
      return () => clearTimeout(timer);
    } else {
      setIsLoading(false);
    }
  }, [missionId, logs.length, activeMission?.status, fetchMissionLogs]);

  return (
    <div className="h-full flex flex-col">
      <Tabs defaultValue="activity" className="h-full flex flex-col">
        <TabsList className="grid w-full grid-cols-3 bg-secondary">
          <TabsTrigger value="activity" className="flex items-center gap-2">
            Activity Log
          </TabsTrigger>
          <TabsTrigger value="status">Agent Status</TabsTrigger>
          <TabsTrigger value="stats">Statistics</TabsTrigger>
        </TabsList>
        
        <TabsContent value="activity" className="flex-1 mt-2 overflow-hidden">
          <AgentActivityLog 
            logs={memoizedLogs}
            isLoading={isLoading}
            missionStatus={activeMission?.status}
            missionId={missionId}
            hasMore={hasMoreLogs}
            onLoadMore={onLoadMoreLogs}
            onLoadAll={onLoadAllLogs}
            isLoadingMore={isLoadingMoreLogs}
            totalLogs={totalLogsCount}
          />
        </TabsContent>
        
        <TabsContent value="status" className="flex-1 mt-2 overflow-hidden">
          <div className="h-full overflow-auto">
            <AgentStatusIndicator 
              logs={memoizedLogs}
              missionStatus={activeMission?.status}
            />
          </div>
        </TabsContent>
        
        <TabsContent value="stats" className="flex-1 mt-2 overflow-hidden">
          <div className="h-full overflow-auto">
            <MissionStatsDashboard 
              logs={memoizedLogs}
              missionStatus={activeMission?.status}
            />
          </div>
        </TabsContent>
      </Tabs>
    </div>
  );
};
