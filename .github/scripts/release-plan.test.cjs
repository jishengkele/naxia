const assert = require("node:assert/strict");
const test = require("node:test");
const { nextVersion, passedCi, reserveRelease } = require("./release-plan.cjs");

test("versions start at 1.0.0 and carry after 99", () => {
  for (const [tags, expected] of [
    [[], "v1.0.0"], [["v0.3.24", "v1.0.0-rc1"], "v1.0.0"],
    [["v1.0.0"], "v1.0.1"], [["v1.0.99"], "v1.1.0"],
    [["v1.99.99"], "v2.0.0"], [["v1.0.9", "v1.0.10", "v0.9.99"], "v1.0.11"],
    [["v2.0.0", "v1.99.99"], "v2.0.1"], [["v99.99.99"], "v100.0.0"],
  ]) assert.equal(nextVersion(tags), expected);
  assert.throws(() => nextVersion(["v1.0.100"]), /进位规则/);
  assert.throws(() => nextVersion(["v1.100.0"]), /进位规则/);
});

const green = { id: 1, head_sha: "commit", head_branch: "main", event: "push", status: "completed", conclusion: "success", html_url: "https://example.invalid/ci/1" };
test("CI must match the commit, trusted event, main branch and latest run", () => {
  assert.equal(passedCi([green], "commit"), green);
  for (const runs of [[], [{ ...green, head_sha: "other" }], [{ ...green, event: "pull_request" }],
    [{ ...green, head_branch: "feature" }], [green, { ...green, id: 2, status: "in_progress", conclusion: null }],
    [green, { ...green, id: 3, conclusion: "failure" }]]) assert.throws(() => passedCi(runs, "commit"));
});

function fixture(options = {}) {
  const refs = [], outputs = {};
  let attempts = 0;
  const actions = { listWorkflowRuns() {} };
  const repos = { listTags() {}, async getBranch() { return { data: { commit: { sha: options.head || "commit" } } }; } };
  const github = { rest: { actions, repos, git: { async createRef(ref) {
    attempts++;
    if (options.collide && attempts === 1) { const error = new Error("exists"); error.status = 422; throw error; }
    refs.push(ref);
  } } }, async paginate(method) {
    if (method === actions.listWorkflowRuns) return options.runs || [green];
    return options.collide && attempts ? [{ name: "v1.0.0" }] : [];
  } };
  const summary = { addHeading() { return this; }, addLink() { return this; }, addRaw() { return this; }, async write() {} };
  return { github, context: { eventName: "workflow_dispatch", ref: "refs/heads/main", sha: "commit", repo: { owner: "owner", repo: "repo" } }, core: { setOutput(key, value) { outputs[key] = value; }, summary }, refs, outputs };
}

test("release cannot reserve a tag before CI passes or from stale main", async () => {
  for (const options of [{ runs: [] }, { head: "newer" }]) {
    const f = fixture(options);
    await assert.rejects(reserveRelease(f));
    assert.equal(f.refs.length, 0);
  }
  const f = fixture(); f.context.ref = "refs/heads/feature";
  await assert.rejects(reserveRelease(f)); assert.equal(f.refs.length, 0);
});

test("tag allocation pins the tested commit and handles a competing tag without force", async () => {
  const f = fixture({ collide: true });
  assert.equal(await reserveRelease(f), "v1.0.1");
  assert.deepEqual(f.refs, [{ owner: "owner", repo: "repo", ref: "refs/tags/v1.0.1", sha: "commit" }]);
  assert.equal(f.outputs.sha, "commit");
});
