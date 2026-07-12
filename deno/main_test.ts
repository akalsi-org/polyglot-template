import { greeting } from "./main.ts";

Deno.test("greeting is deterministic", () => {
  if (greeting("world") !== "hello, world") {
    throw new Error("unexpected greeting");
  }
});
