import { watchApprovalStatus } from "./approval-status.ts";
import assert from "node:assert/strict";
import test from "node:test";
import { setImmediate } from "node:timers/promises";

for (const status of ["allowed", "denied", "timeout"]) {
  test(`external ${status} replaces pending state and stops polling`, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    let calls = 0;
    const updates = [];
    const stop = watchApprovalStatus(
      42,
      async (id) => {
        assert.equal(id, 42);
        return { id, status: ++calls === 1 ? "pending" : status };
      },
      (row) => updates.push(row.status),
      assert.fail,
    );
    t.after(stop);
    await setImmediate();
    assert.deepEqual(updates, ["pending"]);
    t.mock.timers.tick(2000);
    await setImmediate();
    assert.deepEqual(updates, ["pending", status]);
    t.mock.timers.tick(10000);
    await setImmediate();
    assert.equal(calls, 2);
  });
}

test("a transient request failure retries and observes a later decision", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let calls = 0;
  const errors = [];
  const updates = [];
  const stop = watchApprovalStatus(
    42,
    async () => {
      if (++calls === 1) throw new Error("temporarily offline");
      return { id: 42, status: "allowed" };
    },
    (row) => updates.push(row.status),
    (error) => errors.push(error.message),
  );
  t.after(stop);
  await setImmediate();
  assert.deepEqual(errors, ["temporarily offline"]);
  t.mock.timers.tick(2000);
  await setImmediate();
  assert.deepEqual(updates, ["allowed"]);
  assert.equal(calls, 2);
});

test("unmount clears a scheduled refresh", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let calls = 0;
  const stop = watchApprovalStatus(
    42,
    async () => {
      calls++;
      return { id: 42, status: "pending" };
    },
    () => undefined,
    assert.fail,
  );
  await setImmediate();
  stop();
  t.mock.timers.tick(10000);
  await setImmediate();
  assert.equal(calls, 1);
});

test("unmount ignores a late response and does not schedule another request", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let resolve;
  let calls = 0;
  const updates = [];
  const stop = watchApprovalStatus(
    42,
    () => {
      calls++;
      return new Promise((done) => {
        resolve = done;
      });
    },
    (row) => updates.push(row),
    assert.fail,
  );
  stop();
  resolve({ id: 42, status: "pending" });
  await setImmediate();
  t.mock.timers.tick(10000);
  await setImmediate();
  assert.deepEqual(updates, []);
  assert.equal(calls, 1);
});

test("slow requests never overlap and late failures after unmount stay silent", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let reject;
  let calls = 0;
  const errors = [];
  const stop = watchApprovalStatus(
    42,
    () => {
      calls++;
      return new Promise((_, fail) => {
        reject = fail;
      });
    },
    assert.fail,
    (error) => errors.push(error),
  );
  t.mock.timers.tick(10000);
  await setImmediate();
  assert.equal(calls, 1);
  stop();
  reject(new Error("late offline"));
  await setImmediate();
  t.mock.timers.tick(10000);
  assert.deepEqual(errors, []);
  assert.equal(calls, 1);
});
