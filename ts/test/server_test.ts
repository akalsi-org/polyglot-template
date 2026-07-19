import { handleRequest, main } from "../app/server/main.ts";

Deno.test("GET / returns greeting", async () => {
  const req = new Request("http://localhost/");
  const res = handleRequest(req);
  const body = await res.text();
  if (body !== "hello, world") {
    throw new Error(`expected "hello, world", got "${body}"`);
  }
  if (res.status !== 200) {
    throw new Error(`expected status 200, got ${res.status}`);
  }
});

Deno.test("GET /healthz returns ok", async () => {
  const req = new Request("http://localhost/healthz");
  const res = handleRequest(req);
  const body = await res.text();
  if (body !== "ok") {
    throw new Error(`expected "ok", got "${body}"`);
  }
  if (res.status !== 200) {
    throw new Error(`expected status 200, got ${res.status}`);
  }
});

Deno.test("other routes return 404", () => {
  const req = new Request("http://localhost/notfound");
  const res = handleRequest(req);
  if (res.status !== 404) {
    throw new Error(`expected status 404, got ${res.status}`);
  }
});

Deno.test("server main handles the smoke invocation", async () => {
  await main(["--smoke"]);
});
