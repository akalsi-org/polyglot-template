import { nanoid } from "nanoid";
import { greeting } from "@/greeting/greeting.ts";

export function handleRequest(request: Request): Response {
  const url = new URL(request.url);
  if (url.pathname === "/") {
    return new Response(greeting("world"));
  }
  if (url.pathname === "/healthz") {
    return new Response("ok");
  }
  return new Response("not found", { status: 404 });
}

export async function main(args = Deno.args): Promise<void> {
  if (args.includes("--smoke")) {
    // Resolve and execute a locked npm dependency without changing the smoke
    // response contract that package tests assert.
    nanoid(0);
    const response = handleRequest(new Request("http://localhost/"));
    console.log(await response.text());
    return;
  }
  Deno.serve(handleRequest);
}

if (import.meta.main) {
  await main();
}
