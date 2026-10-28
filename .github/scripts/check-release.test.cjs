const assert = require("node:assert/strict");
const test = require("node:test");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

test("publishing guard accepts only the pinned unpublished or matching draft release", t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "naxia-publish-guard-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(dir, "gh"), `#!/usr/bin/env bash
if [[ "$1" == api ]]; then echo "$FIXTURE_SHA"; exit 0; fi
case "$FIXTURE_RELEASE" in
  missing) echo 'release not found' >&2; exit 1 ;;
  error) echo 'HTTP 500' >&2; exit 1 ;;
  draft) echo '{"isDraft":true,"targetCommitish":"commit"}' ;;
  published) echo '{"isDraft":false,"targetCommitish":"commit"}' ;;
esac
`, { mode: 0o700 });
  for (const [sha, release, succeeds] of [["commit", "missing", true], ["commit", "draft", true],
    ["moved", "missing", false], ["commit", "published", false], ["commit", "error", false]]) {
    const result = spawnSync("bash", [path.join(__dirname, "check-release.sh")], {
      encoding: "utf8", env: { ...process.env, PATH: `${dir}${path.delimiter}${process.env.PATH}`,
        RELEASE_TAG: "v1.0.0", RELEASE_COMMIT: "commit", GH_REPO: "owner/repo", FIXTURE_SHA: sha, FIXTURE_RELEASE: release },
    });
    assert.equal(result.status === 0, succeeds, `${sha}/${release}: ${result.stdout}\n${result.stderr}`);
  }
});
