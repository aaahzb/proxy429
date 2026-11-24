# Console usage doc sources

These fragments are the source of truth for the usage doc behind the web console's docs button.
`internal/logview/doc.go` is generated from them and must never be edited by hand.

- `doc-zh.html`: Chinese body, embedded as `logViewerDocZH`.
- `doc-en.html`: English body, embedded as `logViewerDocEN`.

Both must keep the same `<section class="docSec" id="...">` ids and data-group boundaries; the
`internal/logview` tests enforce that parity, require the English body to contain no Han text, and
require every config-example key to have a reference-table row (`<tr><td><code>key</code></td>`) in
both languages.

Regenerate after editing:

```sh
go run ./tools/docgen          # rewrite internal/logview/doc.go
go run ./tools/docgen -check   # fail when doc.go is stale
```

`internal/docgen` re-renders the file in `go test` and fails on drift, so a manual edit to
`doc.go` or a forgotten regeneration cannot land. The Go generator replaced the old local-only
`Temp/gen_doc.py`; why Go: it shares one implementation with the drift test, so the generator and
its guard can never disagree.
