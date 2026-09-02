import type { Memory } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { mockMemories } from "../data";
import { delay, requireSession } from "./shared";

// Agent memory (PRD #90). user_id is mock-internal (the wire Memory carries none —
// the server owner-scopes it), stripped before responding.
type OwnedMemory = Memory & { user_id: string };
let memories: OwnedMemory[] = mockMemories.map((m) => ({ ...m }));
const stripMemoryOwner = ({ user_id: _user_id, ...m }: OwnedMemory): Memory => m;

export const memoryApi = {
  // ── Agent memory (PRD #90 M6) ──────────────────────────────────────────────
  // list is owner-scoped + newest-first (the real endpoint filters `WHERE
  // user_id=$1 ORDER BY created_at DESC`); delete is an owner-scoped purge.
  listMemory: async () => {
    const me = requireSession();
    const mine = memories
      .filter((m) => m.user_id === me.id)
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
      .map(stripMemoryOwner);
    return delay({ memories: mine });
  },
  deleteMemory: async (id: string) => {
    const me = requireSession();
    // Owner-scoped: a foreign id is a 404, exactly like the server.
    const m = memories.find((x) => x.id === id && x.user_id === me.id);
    if (!m) throw new ApiError(404, "memory not found");
    memories = memories.filter((x) => x.id !== id);
    return delay(null);
  },
};
