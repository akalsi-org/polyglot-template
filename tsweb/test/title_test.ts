import { pageTitle } from "#/title/title.ts";

Deno.test("page title is deterministic", () => {
  if (pageTitle("docs") !== "docs | polyglot-template") {
    throw new Error("unexpected page title");
  }
});
