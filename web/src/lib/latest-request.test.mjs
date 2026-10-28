import { createLatestRequest } from "./latest-request.ts";
import assert from "node:assert/strict";
import test from "node:test";

for (const change of ["input edit", "save", "capture toggle"]) {
  test(`${change} invalidates an in-flight result and immediately resets loading`, () => {
    let busy = false;
    const requests = createLatestRequest((value) => {
      busy = value;
    });
    const pending = requests.begin();
    assert.equal(busy, true);
    requests.invalidate();
    assert.equal(requests.isCurrent(pending), false);
    assert.equal(busy, false);
    requests.finish(pending);
    assert.equal(busy, false);
  });
}

test("completion of a stale request cannot unlock a newer request", () => {
  let busy = false;
  const requests = createLatestRequest((value) => {
    busy = value;
  });
  const old = requests.begin();
  requests.invalidate();
  const current = requests.begin();
  requests.finish(old);
  assert.equal(busy, true);
  assert.equal(requests.isCurrent(old), false);
  assert.equal(requests.isCurrent(current), true);
  requests.finish(current);
  assert.equal(busy, false);
});
