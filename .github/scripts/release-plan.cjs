"use strict";

function parseVersion(tag) {
  const match = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(tag);
  if (!match) return null;
  const parts = match.slice(1).map(BigInt);
  if (parts[0] < 1n) return null; // Upstream 0.x tags are not naxia releases.
  if (parts[1] > 99n || parts[2] > 99n) {
    throw new Error(`标签 ${tag} 不符合次版本/修订号 0–99 的进位规则`);
  }
  return parts;
}

function nextVersion(tags) {
  let latest = null;
  for (const tag of tags) {
    const parts = parseVersion(tag);
    if (!parts) continue;
    if (!latest || parts[0] > latest[0] ||
        (parts[0] === latest[0] && parts[1] > latest[1]) ||
        (parts[0] === latest[0] && parts[1] === latest[1] && parts[2] > latest[2])) latest = parts;
  }
  if (!latest) return "v1.0.0";
  let [major, minor, patch] = latest;
  patch++;
  if (patch > 99n) { patch = 0n; minor++; }
  if (minor > 99n) { minor = 0n; major++; }
  return `v${major}.${minor}.${patch}`;
}

function passedCi(runs, sha) {
  const eligible = runs.filter(run => run.head_sha === sha && run.head_branch === "main" &&
    ["push", "workflow_dispatch"].includes(run.event));
  eligible.sort((a, b) => b.id - a.id);
  const latest = eligible[0];
  if (!latest || latest.status !== "completed" || latest.conclusion !== "success") {
    throw new Error("所选 main 提交尚未通过最新一轮 CI，请等待 CI 成功后再发起发布");
  }
  return latest;
}

async function reserveRelease({ github, context, core }) {
  if (context.eventName !== "workflow_dispatch" || context.ref !== "refs/heads/main") {
    throw new Error("只能从 main 分支手动发起发布");
  }
  const { owner, repo } = context.repo;
  const { data: branch } = await github.rest.repos.getBranch({ owner, repo, branch: "main" });
  if (branch.commit.sha !== context.sha) throw new Error("main 已更新，请从最新 main 重新发起发布");
  const runs = await github.paginate(github.rest.actions.listWorkflowRuns, {
    owner, repo, workflow_id: "ci.yml", head_sha: context.sha, branch: "main", per_page: 100,
  });
  const ci = passedCi(runs, context.sha);
  for (let attempt = 0; attempt < 3; attempt++) {
    const tags = await github.paginate(github.rest.repos.listTags, { owner, repo, per_page: 100 });
    const tag = nextVersion(tags.map(item => item.name));
    try {
      await github.rest.git.createRef({ owner, repo, ref: `refs/tags/${tag}`, sha: context.sha });
      core.setOutput("tag", tag);
      core.setOutput("sha", context.sha);
      await core.summary.addHeading(`准备发布 ${tag}`)
        .addLink("已通过的 CI", ci.html_url)
        .addRaw(`\n\n提交：${context.sha}\n\n版本已分配；失败后不复用此版本。\n`).write();
      return tag;
    } catch (error) {
      if (error.status !== 422 || attempt === 2) throw error;
      // Another authorized writer may have created this ref. Never force it.
    }
  }
  throw new Error("无法分配发布版本");
}

module.exports = { parseVersion, nextVersion, passedCi, reserveRelease };
