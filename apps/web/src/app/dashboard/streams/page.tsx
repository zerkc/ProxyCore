"use client";

import { useDashboard } from "../dashboard-context";
import { StreamsView } from "./StreamsView";

export default function DashboardStreamsPage() {
  const { status, saveStream, updateStream, deleteStream } = useDashboard();

  return (
    <StreamsView
      streams={status?.streams ?? []}
      saveStream={saveStream}
      updateStream={updateStream}
      deleteStream={deleteStream}
    />
  );
}
