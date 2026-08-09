"use client";

import {
  createContext,
  FormEvent,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useRouter } from "next/navigation";
import type { EditableRecord } from "./RecordDialog";
import type { StatusPayload, Zone } from "./types";

type DashboardContextValue = {
  status?: StatusPayload;
  message: string;
  error: string;
  setMessage: (value: string) => void;
  setError: (value: string) => void;
  zoneName: string;
  setZoneName: (value: string) => void;
  selectedZone: string;
  setSelectedZone: (value: string) => void;
  activeZone?: Zone;
  ingressIpv4: string;
  setIngressIpv4: (value: string) => void;
  resolver: string;
  setResolver: (value: string) => void;
  recordDialogOpen: boolean;
  editingRecord?: EditableRecord;
  openCreateRecord: () => void;
  openEditRecord: (record: EditableRecord) => void;
  closeRecordDialog: () => void;
  refresh: () => Promise<void>;
  createZone: (event: FormEvent<HTMLFormElement>) => Promise<void>;
  saveRecord: (payload: Record<string, unknown>) => Promise<boolean>;
  saveNetwork: (event: FormEvent<HTMLFormElement>) => Promise<void>;
  saveStream: (payload: Record<string, unknown>) => Promise<boolean>;
  updateStream: (
    streamId: string,
    payload: Record<string, unknown>,
  ) => Promise<boolean>;
  deleteStream: (streamId: string) => Promise<boolean>;
  apply: () => Promise<void>;
  logout: () => Promise<void>;
  inSync: boolean;
};

const DashboardContext = createContext<DashboardContextValue | null>(null);

export function DashboardProvider({ children }: { children: ReactNode }) {
  const router = useRouter();
  const [status, setStatus] = useState<StatusPayload>();
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [zoneName, setZoneName] = useState("home.arpa");
  const [selectedZone, setSelectedZone] = useState("");
  const [recordDialogOpen, setRecordDialogOpen] = useState(false);
  const [editingRecord, setEditingRecord] = useState<EditableRecord>();
  const [ingressIpv4, setIngressIpv4] = useState("");
  const [resolver, setResolver] = useState("192.168.1.1");

  const activeZone = useMemo(
    () => status?.zones.find((zone) => zone.id === selectedZone),
    [selectedZone, status?.zones],
  );

  const inSync =
    !!status?.desiredRevision &&
    !!status?.appliedRevision &&
    status.desiredRevision.checksum === status.appliedRevision.checksum;

  async function refresh() {
    const response = await fetch("/api/status", { cache: "no-store" });
    if (response.status === 401) {
      router.push("/login");
      return;
    }
    if (!response.ok) {
      setError("Status could not be loaded");
      return;
    }
    const payload = (await response.json()) as StatusPayload;
    setStatus(payload);
    setIngressIpv4(payload.settings.ingress.ipv4 ?? "");
    setResolver(
      payload.settings.defaultPool?.endpoints[0]?.host ?? "192.168.1.1",
    );
    setSelectedZone((current) =>
      current && payload.zones.some((zone) => zone.id === current)
        ? current
        : "",
    );
  }

  useEffect(() => {
    void refresh();
  }, []);

  async function createZone(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setMessage("");
    setError("");
    const response = await fetch("/api/zones", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: zoneName }),
    });
    if (response.status === 401) {
      router.push("/login");
      return;
    }
    const payload = (await response.json().catch(() => ({}))) as {
      error?: string;
      zone?: { id: string };
    };
    if (!response.ok) {
      setError(payload.error ?? "Change rejected");
      return;
    }
    setMessage("Zone created");
    await refresh();
    if (payload.zone?.id) {
      setSelectedZone(payload.zone.id);
      router.push(`/dashboard/dns/${payload.zone.id}`);
    }
  }

  async function saveRecord(
    payload: Record<string, unknown>,
  ): Promise<boolean> {
    if (!activeZone) {
      setError("Create a zone first");
      return false;
    }
    const path = editingRecord
      ? `/api/zones/${activeZone.id}/records/${editingRecord.id}`
      : `/api/zones/${activeZone.id}/records`;
    const saved = await mutate(
      path,
      payload,
      editingRecord ? "Record updated" : "Record saved",
      editingRecord ? "PATCH" : "POST",
    );
    if (saved) {
      setRecordDialogOpen(false);
      setEditingRecord(undefined);
    }
    return saved;
  }

  async function saveNetwork(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await mutate(
      "/api/settings",
      {
        ingress: { ipv4: ingressIpv4 || undefined },
        defaultPool: {
          id: "default",
          endpoints: [{ host: resolver, port: 53 }],
        },
      },
      "Network settings saved",
      "PUT",
    );
  }

  async function saveStream(
    payload: Record<string, unknown>,
  ): Promise<boolean> {
    const saved = await mutate("/api/streams", payload, "Stream route saved");
    if (saved) {
      await mutate("/api/apply", {}, "Stream route saved and apply queued");
    }
    return saved;
  }

  async function updateStream(
    streamId: string,
    payload: Record<string, unknown>,
  ): Promise<boolean> {
    const saved = await mutate(
      `/api/streams/${streamId}`,
      payload,
      "Stream updated",
      "PATCH",
    );
    if (saved) {
      await mutate("/api/apply", {}, "Stream updated and apply queued");
    }
    return saved;
  }

  async function deleteStream(streamId: string): Promise<boolean> {
    const deleted = await mutate(
      `/api/streams/${streamId}`,
      undefined,
      "Stream deleted",
      "DELETE",
    );
    if (deleted) {
      await mutate("/api/apply", {}, "Stream deleted and apply queued");
    }
    return deleted;
  }

  async function apply() {
    await mutate("/api/apply", {}, "Apply queued");
  }

  async function mutate(
    path: string,
    body: unknown,
    success: string,
    method = "POST",
  ): Promise<boolean> {
    setMessage("");
    setError("");
    const response = await fetch(path, {
      method,
      ...(body === undefined
        ? {}
        : {
            headers: { "content-type": "application/json" },
            body: JSON.stringify(body),
          }),
    });
    if (response.status === 401) {
      router.push("/login");
      return false;
    }
    if (!response.ok) {
      setError((await response.json()).error ?? "Change rejected");
      return false;
    }
    setMessage(success);
    await refresh();
    return true;
  }

  async function logout() {
    await fetch("/api/auth/logout", { method: "POST" });
    router.push("/login");
  }

  const value: DashboardContextValue = {
    status,
    message,
    error,
    setMessage,
    setError,
    zoneName,
    setZoneName,
    selectedZone,
    setSelectedZone,
    activeZone,
    ingressIpv4,
    setIngressIpv4,
    resolver,
    setResolver,
    recordDialogOpen,
    editingRecord,
    openCreateRecord: () => {
      setEditingRecord(undefined);
      setRecordDialogOpen(true);
    },
    openEditRecord: (record) => {
      setEditingRecord(record);
      setRecordDialogOpen(true);
    },
    closeRecordDialog: () => {
      setRecordDialogOpen(false);
      setEditingRecord(undefined);
    },
    refresh,
    createZone,
    saveRecord,
    saveNetwork,
    saveStream,
    updateStream,
    deleteStream,
    apply,
    logout,
    inSync,
  };

  return (
    <DashboardContext.Provider value={value}>
      {children}
    </DashboardContext.Provider>
  );
}

export function useDashboard() {
  const context = useContext(DashboardContext);
  if (!context) {
    throw new Error("useDashboard must be used within DashboardProvider");
  }
  return context;
}
