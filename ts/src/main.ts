import cluster from "node:cluster";
import { config } from "./config.js";

if (config.workers > 1 && cluster.isPrimary) {
  for (let i = 0; i < config.workers; i++) cluster.fork();
  cluster.on("exit", (w, code) => console.error(`worker ${w.process.pid} exited with ${code}`));
} else {
  const { buildApp } = await import("./app.js");
  const app = buildApp();
  await app.listen({ port: config.port, host: "0.0.0.0" });
  console.log(`listening on :${config.port} (pid ${process.pid})`);
}
