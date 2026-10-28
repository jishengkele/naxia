import type { InterceptPending, Session } from "./types";

// Approval identity is independent of the currently selected transcript. Older
// main-agent approvals without a segment belong to the original session only.
export function approvalBelongsToSession(
  approval: Pick<InterceptPending, "agent_name" | "session">,
  session: Pick<Session, "role" | "seg" | "intent_id" | "inherited">,
): boolean {
  if (session.inherited) return false;
  if (session.role === "mainagent") {
    return approval.agent_name === "mainagent" && (approval.session || "main:0") === `main:${session.seg ?? 0}`;
  }
  if (session.role === "planner") return approval.agent_name === "planner";
  if (session.role !== "worker" || !session.intent_id) return false;
  const match = approval.agent_name.match(/·\s*#(\d+)$/);
  return match?.[1] === session.intent_id;
}
