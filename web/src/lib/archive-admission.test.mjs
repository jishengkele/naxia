import assert from "node:assert/strict";
import test from "node:test";
import { archiveAdmissionBlocked } from "./archive-admission.ts";

test("archive confirmation waits for capacity and refuses any unknown writer", () => {
  assert.equal(archiveAdmissionBlocked(null), true);
  assert.equal(archiveAdmissionBlocked({ sufficient: false, writers: [] }), true);
  assert.equal(archiveAdmissionBlocked({ sufficient: true, writers: [{ managed: true }, { managed: false }] }), true);
  assert.equal(archiveAdmissionBlocked({ sufficient: true, writers: [{ managed: true }] }), false);
  assert.equal(archiveAdmissionBlocked({ sufficient: true }), false);
});
