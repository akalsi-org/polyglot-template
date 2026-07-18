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

if (import.meta.main) {
  Deno.serve(handleRequest);
}
