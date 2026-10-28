import { approvalBelongsToSession } from "./approval-session.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("main-agent pending approvals belong to their exact conversation segment", () => {
  const approval = { agent_name: "mainagent", session: "main:3" };
  assert.equal(approvalBelongsToSession(approval, { role: "mainagent", seg: 3 }), true);
  assert.equal(approvalBelongsToSession(approval, { role: "mainagent", seg: 0 }), false);
  assert.equal(approvalBelongsToSession(approval, { role: "mainagent", seg: 4 }), false);
  assert.equal(approvalBelongsToSession(approval, { role: "planner" }), false);
});

test("legacy main-agent approvals fall back only to the original session", () => {
  const approval = { agent_name: "mainagent" };
  assert.equal(approvalBelongsToSession(approval, { role: "mainagent" }), true);
  assert.equal(approvalBelongsToSession(approval, { role: "mainagent", seg: 0 }), true);
  assert.equal(approvalBelongsToSession(approval, { role: "mainagent", seg: 1 }), false);
});

test("planner and worker ownership retain their existing role and intent semantics", () => {
  assert.equal(approvalBelongsToSession({ agent_name: "planner", session: "plan" }, { role: "planner" }), true);
  const approval = { agent_name: "work#2 · #42" };
  assert.equal(approvalBelongsToSession(approval, { role: "worker", intent_id: "42" }), true);
  assert.equal(approvalBelongsToSession(approval, { role: "worker", intent_id: "43" }), false);
  assert.equal(approvalBelongsToSession(approval, { role: "worker" }), false);
  assert.equal(approvalBelongsToSession(approval, { role: "system", intent_id: "42" }), false);
});

test("inherited sessions never advertise actionable pending approvals", () => {
  for (const [approval, session] of [
    [
      { agent_name: "mainagent", session: "main:3" },
      { role: "mainagent", seg: 3 },
    ],
    [{ agent_name: "planner" }, { role: "planner" }],
    [{ agent_name: "work#2 · #42" }, { role: "worker", intent_id: "42" }],
  ]) {
    assert.equal(approvalBelongsToSession(approval, { ...session, inherited: true }), false);
  }
});
