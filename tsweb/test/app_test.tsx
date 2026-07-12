/// <reference types="npm:@types/react@19.2.17" />

import { App } from "#/app/app.tsx";

Deno.test("React app has a semantic root", () => {
  const element = App();
  if (element.type !== "main") {
    throw new Error("expected the React application root to be <main>");
  }
});
