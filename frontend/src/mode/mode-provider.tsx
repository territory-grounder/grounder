import { createContext, useContext, type ReactNode } from "react";
import { type MutationMode, modeFromServer } from "./mutation-mode";

// The provider defaults to "read-only": before the server value is known, and whenever mutation is off,
// every panel sees read-only mode. Turning operational requires the server to report mutation_enabled=true.
const MutationModeContext = createContext<MutationMode>("read-only");

export function MutationModeProvider({
  mutationEnabled,
  children,
}: {
  mutationEnabled?: boolean | null;
  children: ReactNode;
}) {
  return (
    <MutationModeContext.Provider value={modeFromServer(mutationEnabled)}>
      {children}
    </MutationModeContext.Provider>
  );
}

export function useMutationMode(): MutationMode {
  return useContext(MutationModeContext);
}
