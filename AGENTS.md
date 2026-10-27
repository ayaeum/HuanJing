# HuanJing implementation

- Keep changes in logical commits that can be reviewed and rolled back independently.
- Preserve independent LIKE and COMMENT actions and their per-action idempotency.
- Treat docs/design.md as the target design. Clearly document local substitutions and incomplete production features.
- Never claim mock model quality or local benchmark throughput as real provider capacity.
- Run meaningful correctness and failure/restart checks before claiming completion.
- For browser operations prefer ego-browser.
