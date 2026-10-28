import { parseMaxConcurrency } from "./llm-limits.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("concurrency defaults to unlimited and accepts non-negative integers", () => {
  for (const [input, value] of [
    ["", 0],
    ["  ", 0],
    ["0", 0],
    ["3", 3],
    [" 12 ", 12],
    ["2147483647", 2147483647],
  ]) {
    assert.equal(parseMaxConcurrency(input), value);
  }
});

test("invalid limits cannot silently disable concurrency protection", () => {
  for (const input of ["-1", "0.5", "NaN", "Infinity", "oops", "2147483648"]) {
    assert.throws(() => parseMaxConcurrency(input));
  }
});
