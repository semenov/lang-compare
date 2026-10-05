import http from "node:http";
import { initPool, migrate, pool } from "./db.js";
import { ApiError, type Ctx, type Result, notFound, parseJson, sendProblem, sendResult } from "./http.js";
import * as auth from "./auth.js";
import * as orgs from "./orgs.js";
import * as issues from "./issues.js";
import * as hooks from "./webhooks.js";

type Handler = (c: Ctx) => Promise<Result>;
interface Route {
  method: string;
  re: RegExp;
  handler: Handler;
  public?: boolean;
}

const routes: Route[] = [];
function route(method: string, pattern: string, handler: Handler, isPublic = false) {
  const re = new RegExp("^" + pattern.replace(/\{[a-z_]+\}/g, "([^/]+)") + "/?$");
  routes.push({ method, re, handler, public: isPublic });
}

const A = "/api/v1";
route("GET", "/healthz", async () => ({ status: 200, body: { status: "ok" } }), true);
route(
  "GET",
  "/readyz",
  async () => {
    try {
      await pool.query("SELECT 1");
      return { status: 200, body: { status: "ok" } };
    } catch {
      return { status: 503, body: { status: "unavailable" } };
    }
  },
  true,
);
route("POST", `${A}/auth/register`, auth.register, true);
route("POST", `${A}/auth/login`, auth.login, true);
route("POST", `${A}/auth/refresh`, auth.refresh, true);
route("POST", `${A}/auth/logout`, auth.logout, true);
route("GET", `${A}/me`, auth.getMe);
route("PATCH", `${A}/me`, auth.patchMe);

route("POST", `${A}/orgs`, orgs.createOrg);
route("GET", `${A}/orgs`, orgs.listOrgs);
route("GET", `${A}/orgs/{slug}`, orgs.getOrg);
route("GET", `${A}/orgs/{slug}/members`, orgs.listMembers);
route("POST", `${A}/orgs/{slug}/members`, orgs.addMember);
route("PATCH", `${A}/orgs/{slug}/members/{id}`, orgs.patchMember);
route("DELETE", `${A}/orgs/{slug}/members/{id}`, orgs.deleteMember);

route("POST", `${A}/orgs/{slug}/projects`, orgs.createProject);
route("GET", `${A}/orgs/{slug}/projects`, orgs.listProjects);
route("GET", `${A}/orgs/{slug}/projects/{key}`, orgs.getProject);
route("PATCH", `${A}/orgs/{slug}/projects/{key}`, orgs.patchProject);
route("DELETE", `${A}/orgs/{slug}/projects/{key}`, orgs.deleteProject);
route("GET", `${A}/orgs/{slug}/projects/{key}/members`, orgs.listProjectMembers);
route("PUT", `${A}/orgs/{slug}/projects/{key}/members/{id}`, orgs.putProjectMember);
route("DELETE", `${A}/orgs/{slug}/projects/{key}/members/{id}`, orgs.deleteProjectMember);

route("POST", `${A}/orgs/{slug}/projects/{key}/issues`, issues.createIssue);
route("POST", `${A}/orgs/{slug}/projects/{key}/issues/bulk`, issues.bulkCreate);
route("GET", `${A}/orgs/{slug}/issues`, issues.listIssues);
route("GET", `${A}/orgs/{slug}/issues/{key}`, issues.getIssue);
route("PATCH", `${A}/orgs/{slug}/issues/{key}`, issues.patchIssue);
route("DELETE", `${A}/orgs/{slug}/issues/{key}`, issues.deleteIssue);
route("POST", `${A}/orgs/{slug}/issues/{key}/transition`, issues.transitionIssue);
route("POST", `${A}/orgs/{slug}/issues/{key}/comments`, issues.createComment);
route("GET", `${A}/orgs/{slug}/issues/{key}/comments`, issues.listComments);
route("GET", `${A}/orgs/{slug}/issues/{key}/history`, issues.listHistory);
route("PATCH", `${A}/orgs/{slug}/comments/{id}`, issues.patchComment);
route("DELETE", `${A}/orgs/{slug}/comments/{id}`, issues.deleteComment);

route("POST", `${A}/orgs/{slug}/webhooks`, hooks.createHook);
route("GET", `${A}/orgs/{slug}/webhooks`, hooks.listHooks);
route("GET", `${A}/orgs/{slug}/webhooks/{id}`, hooks.getHook);
route("PATCH", `${A}/orgs/{slug}/webhooks/{id}`, hooks.patchHook);
route("DELETE", `${A}/orgs/{slug}/webhooks/{id}`, hooks.deleteHook);
route("GET", `${A}/orgs/{slug}/webhooks/{id}/deliveries`, hooks.listDeliveries);

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on("data", (d: Buffer) => chunks.push(d));
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  try {
    const url = new URL(req.url ?? "/", "http://localhost");
    const method = req.method ?? "GET";
    let match: { r: Route; params: string[] } | null = null;
    let pathMatched = false;
    for (const r of routes) {
      const m = r.re.exec(url.pathname);
      if (!m) continue;
      pathMatched = true;
      if (r.method !== method) continue;
      match = { r, params: m.slice(1).map((s) => decodeURIComponent(s)) };
      break;
    }
    const rawBody = await readBody(req);
    if (!match) {
      if (pathMatched) throw new ApiError(405, "method_not_allowed", "method not allowed");
      throw notFound("no such endpoint");
    }
    let userId = "";
    if (!match.r.public) userId = await auth.authenticate(req.headers.authorization);
    let parsed: Record<string, unknown> | undefined;
    const ctx: Ctx = {
      req,
      method,
      params: match.params,
      query: url.searchParams,
      rawBody,
      userId,
      body() {
        parsed ??= parseJson(rawBody);
        return parsed;
      },
      header(name: string) {
        const h = req.headers[name.toLowerCase()];
        return Array.isArray(h) ? h[0] : h;
      },
    };
    if (["POST", "PATCH", "PUT"].includes(method)) ctx.body();
    const result = await match.r.handler(ctx);
    hooks.kick();
    sendResult(res, result);
  } catch (e) {
    if (e instanceof ApiError) {
      sendProblem(res, e);
    } else {
      console.error("internal error", e);
      sendProblem(res, new ApiError(500, "internal", "internal server error"));
    }
  }
}

async function main() {
  const dbUrl = process.env.DATABASE_URL;
  const secret = process.env.JWT_SECRET;
  if (!dbUrl || !secret) {
    console.error("DATABASE_URL and JWT_SECRET are required");
    process.exit(2);
  }
  auth.setJwtSecret(secret);
  const port = Number(process.env.PORT ?? 8080);
  const scale = Number(process.env.WEBHOOK_BACKOFF_SCALE ?? "1.0");
  initPool(dbUrl);
  for (let i = 0; ; i++) {
    try {
      await migrate();
      break;
    } catch (e) {
      if (i >= 30) throw e;
      await new Promise((r) => setTimeout(r, 500));
    }
  }

  const server = http.createServer((req, res) => {
    void handle(req, res);
  });
  server.keepAliveTimeout = 5000;
  server.listen(port, "0.0.0.0");
  hooks.startWorker(Number.isFinite(scale) && scale >= 0 ? scale : 1);

  let stopping = false;
  const shutdown = async () => {
    if (stopping) return;
    stopping = true;
    const deadline = Date.now() + 10000;
    const closed = new Promise<void>((r) => server.close(() => r()));
    server.closeIdleConnections();
    const force = setTimeout(() => server.closeAllConnections(), 10000);
    await Promise.race([closed, new Promise((r) => setTimeout(r, 10000))]);
    clearTimeout(force);
    await hooks.stopWorker(Math.max(0, Math.min(3000, deadline - Date.now())));
    try {
      await Promise.race([pool.end(), new Promise((r) => setTimeout(r, 1000))]);
    } catch {}
    process.exit(0);
  };
  process.on("SIGTERM", () => void shutdown());
  process.on("SIGINT", () => void shutdown());
  console.log(`tracker listening on ${port}`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
