// Configuration edits invalidate both the old result and its loading state.
// Completion of an older request must never unlock a newer request's button.
export function createLatestRequest(onBusyChange: (busy: boolean) => void) {
  let generation = 0;
  return {
    begin() {
      onBusyChange(true);
      return ++generation;
    },
    isCurrent(token: number) {
      return token === generation;
    },
    finish(token: number) {
      if (token === generation) onBusyChange(false);
    },
    invalidate() {
      generation++;
      onBusyChange(false);
    },
  };
}
