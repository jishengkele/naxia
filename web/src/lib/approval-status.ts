import type { InterceptPending } from "./types";

// Follow a pending approval until it settles, including decisions made elsewhere.
// Schedule after each response to avoid overlapping requests; transient failures
// remain visible and retry without leaving an indefinitely stale approval card.
export function watchApprovalStatus(
  id: number,
  load: (id: number) => Promise<InterceptPending>,
  onUpdate: (approval: InterceptPending) => void,
  onError: (error: unknown) => void,
): () => void {
  let live = true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const refresh = async () => {
    try {
      const approval = await load(id);
      if (!live) return;
      onUpdate(approval);
      if (approval.status !== "pending") return;
    } catch (error) {
      if (!live) return;
      onError(error);
    }
    if (live) timer = setTimeout(() => void refresh(), 2000);
  };
  void refresh();
  return () => {
    live = false;
    clearTimeout(timer);
  };
}
