/// <reference types="npm:@types/react@19.2.17" />
/// <reference types="npm:@types/react-dom@19.2.3" />

import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "#/app/app.tsx";
import { pageTitle } from "#/title/title.ts";
import "./style.css";

document.title = pageTitle("home");

const root = document.getElementById("root");
if (root === null) {
  throw new Error("missing #root element");
}
createRoot(root).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
