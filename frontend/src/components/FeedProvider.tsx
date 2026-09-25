"use client";

import { createContext, useContext, useEffect, useState } from "react";
import { WS_URL } from "@/lib/api";
import { FeedClient } from "@/lib/feed";

const FeedContext = createContext<FeedClient | null>(null);

/** Owns the single FeedClient for the app; disposes sockets/timers on unmount. */
export function FeedProvider({ children }: { children: React.ReactNode }) {
  // Constructing the client has no side effects; connecting happens in the effect.
  const [client] = useState(() => new FeedClient(WS_URL));

  useEffect(() => {
    client.start();
    return () => client.dispose(); // start/dispose are re-entrant (StrictMode double-mount)
  }, [client]);

  return <FeedContext.Provider value={client}>{children}</FeedContext.Provider>;
}

export const useFeed = () => useContext(FeedContext);
