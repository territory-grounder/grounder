import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@fontsource/ibm-plex-sans/400.css";
import "@fontsource/ibm-plex-sans/500.css";
import "@fontsource/ibm-plex-sans/600.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "./styles/theme.css";
import { App } from "./app";
import { AuthGate } from "./auth/AuthGate";

// Fail-closed: AuthGate wraps the entire app. <App> — every panel, its QueryClientProvider, and the
// demo/sample-data.ts fixtures it renders — mounts ONLY once AuthGate has a verified session. Until
// then the tree contains the sign-in screen and nothing else (no panel, no fetch, no fixture).
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <AuthGate>
      <App mutationEnabled={false} />
    </AuthGate>
  </StrictMode>,
);
