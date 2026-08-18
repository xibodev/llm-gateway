import type { JSONRecord } from "../lib/api";
import type { ConsoleMode } from "../lib/mode";
import { ProviderHub } from "../components/providers/ProviderHub";
import { ProviderDetail } from "./ProviderDetail";

export function Providers({ data, mode, detail, onChanged, onNavigate }: {
  data: JSONRecord;
  mode: ConsoleMode;
  detail: string;
  onChanged: () => Promise<void>;
  onNavigate: (page: "providers" | "playground", detail?: string) => void;
}) {
  if (detail) {
    return <ProviderDetail entryID={detail} data={data} mode={mode} onChanged={onChanged} onBack={() => onNavigate("providers")} onOpenPlayground={(modelID) => onNavigate("playground", modelID)} />;
  }
  return <ProviderHub data={data} mode={mode} onChanged={onChanged} onOpenDetail={(entryID) => onNavigate("providers", entryID)} />;
}
