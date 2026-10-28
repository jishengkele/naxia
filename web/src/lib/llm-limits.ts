export function parseMaxConcurrency(raw: string): number {
  const value = Number(raw.trim() || "0");
  if (!Number.isInteger(value) || value < 0 || value > 2_147_483_647) {
    throw new Error("在飞请求上限必须是 0 到 2147483647 之间的整数，0 表示不限");
  }
  return value;
}
