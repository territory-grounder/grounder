import { QueryClient } from "@tanstack/react-query";

// One shared query client. Panels read server state through it; a failed query surfaces the error so the
// panel resolves to the restrictive guarded state (never stale/fabricated data).
export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
}
