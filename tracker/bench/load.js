// k6 scenario: a realistic mix of tracker traffic against a seeded instance.
// env: BASE (http://app:8080), SEED (path to seed.json), MODE ("saturate" | "rate"), VUS, RATE, DURATION, OUT
import http from "k6/http";
import { check } from "k6";
import exec from "k6/execution";

const seed = JSON.parse(open(__ENV.SEED));
const BASE = __ENV.BASE + "/api/v1";
const ORG = seed.org;
const MODE = __ENV.MODE || "saturate";
const OPS = ["list", "search", "view", "create", "update", "transition", "comment", "login"];
const WEIGHTS = [30, 10, 20, 10, 10, 5, 12, 3];

const thresholds = {};
for (const op of OPS) thresholds[`http_req_duration{op:${op}}`] = ["max>=0"];
thresholds["checks"] = ["rate>0.99"];

export const options = {
  scenarios: MODE === "saturate"
    ? { load: { executor: "constant-vus", vus: Number(__ENV.VUS || 64), duration: __ENV.DURATION || "60s" } }
    : { load: { executor: "constant-arrival-rate", rate: Number(__ENV.RATE || 500), timeUnit: "1s",
                duration: __ENV.DURATION || "60s", preAllocatedVUs: 200, maxVUs: 1000 } },
  thresholds,
  summaryTrendStats: ["avg", "p(50)", "p(90)", "p(99)", "p(99.9)", "max", "count"],
  setupTimeout: "300s",
  discardResponseBodies: false,
};

function hdr(token, extra) {
  return { headers: Object.assign({ "Content-Type": "application/json", Authorization: "Bearer " + token }, extra || {}) };
}

export function setup() {
  const tokens = seed.emails.map((email) => {
    const r = http.post(`${BASE}/auth/login`, JSON.stringify({ email, password: seed.password }),
      { headers: { "Content-Type": "application/json" } });
    return r.json("access_token");
  });
  return { tokens };
}

function rnd(n) { return Math.floor(Math.random() * n); }
function pick(a) { return a[rnd(a.length)]; }
function pickOp() {
  let x = rnd(100);
  for (let i = 0; i < OPS.length; i++) { if (x < WEIGHTS[i]) return OPS[i]; x -= WEIGHTS[i]; }
  return OPS[0];
}
function issueKey() { return `${pick(seed.projects)}-${1 + rnd(seed.per_project)}`; }
function words(n) { const w = []; for (let i = 0; i < n; i++) w.push(pick(seed.search_words)); return w.join(" "); }
const NEXT = { todo: ["in_progress"], in_progress: ["todo", "done"], done: ["in_progress"] };

export default function (data) {
  const u = exec.vu.idInTest % data.tokens.length;
  const token = data.tokens[u];
  const op = pickOp();
  const tags = { op, name: op };
  let r;
  switch (op) {
    case "list": {
      const p = [`project=${pick(seed.projects)}`, "status=todo,in_progress", `sort=${pick(["-updated", "-created", "-priority", "key"])}`, "limit=50"];
      const v = rnd(3);
      if (v === 0) p.push("assignee=me");
      if (v === 1) p.push(`label=${pick(seed.labels)}`);
      if (v === 2) p.push("priority=high,highest");
      r = http.get(`${BASE}/orgs/${ORG}/issues?${p.join("&")}`, Object.assign(hdr(token), { tags }));
      check(r, { "list 200": (x) => x.status === 200 });
      break;
    }
    case "search": {
      const q = encodeURIComponent(words(1 + rnd(2)));
      const proj = rnd(2) ? `&project=${pick(seed.projects)}` : "";
      r = http.get(`${BASE}/orgs/${ORG}/issues?q=${q}${proj}&limit=20`, Object.assign(hdr(token), { tags }));
      check(r, { "search 200": (x) => x.status === 200 });
      break;
    }
    case "view": {
      const k = issueKey();
      const rs = http.batch([
        ["GET", `${BASE}/orgs/${ORG}/issues/${k}`, null, Object.assign(hdr(token), { tags })],
        ["GET", `${BASE}/orgs/${ORG}/issues/${k}/comments?limit=50`, null, Object.assign(hdr(token), { tags })],
        ["GET", `${BASE}/orgs/${ORG}/issues/${k}/history?limit=50`, null, Object.assign(hdr(token), { tags })],
      ]);
      check(rs[0], { "view 200": (x) => x.status === 200 });
      break;
    }
    case "create": {
      const body = { type: pick(["task", "bug", "story"]), title: words(6), description: words(40),
        priority: pick(["high", "medium", "low"]), labels: [pick(seed.labels)] };
      r = http.post(`${BASE}/orgs/${ORG}/projects/${pick(seed.projects)}/issues`, JSON.stringify(body),
        Object.assign(hdr(token), { tags }));
      check(r, { "create 201": (x) => x.status === 201 });
      break;
    }
    case "update": {
      const k = issueKey();
      const g = http.get(`${BASE}/orgs/${ORG}/issues/${k}`, Object.assign(hdr(token), { tags }));
      if (g.status !== 200) break;
      r = http.patch(`${BASE}/orgs/${ORG}/issues/${k}`, JSON.stringify({ priority: pick(["highest", "high", "medium", "low"]), title: words(5) }),
        Object.assign(hdr(token, { "If-Match": `"${g.json("version")}"` }), { tags }));
      check(r, { "update 200/412": (x) => x.status === 200 || x.status === 412 });
      break;
    }
    case "transition": {
      const k = issueKey();
      const g = http.get(`${BASE}/orgs/${ORG}/issues/${k}`, Object.assign(hdr(token), { tags }));
      if (g.status !== 200) break;
      r = http.post(`${BASE}/orgs/${ORG}/issues/${k}/transition`, JSON.stringify({ status: pick(NEXT[g.json("status")]) }),
        Object.assign(hdr(token), { tags }));
      check(r, { "transition 200/409": (x) => x.status === 200 || x.status === 409 });
      break;
    }
    case "comment": {
      r = http.post(`${BASE}/orgs/${ORG}/issues/${issueKey()}/comments`, JSON.stringify({ body: words(20) }),
        Object.assign(hdr(token), { tags }));
      check(r, { "comment 201": (x) => x.status === 201 });
      break;
    }
    case "login": {
      r = http.post(`${BASE}/auth/login`, JSON.stringify({ email: seed.emails[u], password: seed.password }),
        { headers: { "Content-Type": "application/json" }, tags });
      check(r, { "login 200": (x) => x.status === 200 });
      break;
    }
  }
}

export function handleSummary(data) {
  const out = { http_reqs: data.metrics.http_reqs.values, iterations: data.metrics.iterations.values,
    checks: data.metrics.checks.values, failed: data.metrics.http_req_failed.values, ops: {} };
  for (const op of OPS) {
    const m = data.metrics[`http_req_duration{op:${op}}`];
    if (m) out.ops[op] = m.values;
  }
  out.all = data.metrics.http_req_duration.values;
  return { [__ENV.OUT]: JSON.stringify(out, null, 1), stdout: `reqs ${out.http_reqs.rate.toFixed(0)}/s, checks ${(out.checks.rate * 100).toFixed(2)}%\n` };
}
