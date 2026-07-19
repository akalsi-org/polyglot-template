/// <reference types="npm:@types/react@19.2.17" />

export function App(): React.JSX.Element {
  return (
    <main>
      <p className="eyebrow">polyglot-template</p>
      <h1>React 19 is ready.</h1>
      <p className="summary">
        Built with pinned Deno and Vite, with no JavaScript runtime required in
        production.
      </p>
    </main>
  );
}
