// Also used by API clients which have only the capacity fields from older servers.
export function archiveAdmissionBlocked(estimate: {
  sufficient: boolean;
  writers?: { managed: boolean }[];
} | null): boolean {
  return !estimate?.sufficient || Boolean(estimate.writers?.some((writer) => !writer.managed));
}
