# Troubleshooting

Use the repository command surface; do not repair `.local/`, `buck-out/`, or
generated outputs by hand.

| Symptom | Safe next command |
| --- | --- |
| Toolchain missing or stale | `./repo.sh bootstrap`, then `./repo.sh doctor --deep` |
| Native Python extension import fails | `./repo.sh python-build`, then run through `./repo.sh python ...` |
| C++ editor diagnostics are stale | `./repo.sh compile-commands`, then reload clangd |
| Deno import/type diagnostic is unresolved | `./repo.sh bootstrap`, then restart the Deno language server |
| Package cannot be built | `./repo.sh package-list`; then `./repo.sh package-target-check <name>` |
| Package fails outside the source tree | `./repo.sh package-smoke <archive> <name>` |
| A normal operation appears to need a download | Stop and inspect `./repo.sh doctor --deep`; only bootstrap may fetch |

When an operation fails, retain its full output. If the error concerns a
declared package with no Buck target, it is intentionally fail-closed rather
than an invitation to run the retired raw-build assembler.
