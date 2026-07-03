# Canvas design checklist

Terse reference for authoring a scrim canvas's HTML/CSS/JS. Load
`artifact-design` (and `dataviz` for any chart/graph) before writing content
if those skills are available — this is a supplement, not a replacement.

- **Name the posture first**: is this canvas a one-off document/report
  (optimize for readability, article-like) or an interactive UI/dashboard
  (optimize for information density, live updates)? Pick one explicitly
  before styling — it drives every other decision below.
- **Theme via tokens, not hardcoded colors**: use `prefers-color-scheme` for
  light/dark rather than baking in one palette.
- **Self-contained by default**: no CDN/external fetches for anything that
  could be inlined. Exception: a canvas that's deliberately a dashboard for a
  live LAN/local service (e.g. visualizing homelab data) legitimately fetches
  from that service — that's the canvas's actual purpose, not a violation.
- **`overflow-x: auto`** on tables, code blocks, and wide diagrams — never let
  the page itself scroll horizontally.
- **`font-variant-numeric: tabular-nums`** (or equivalent) on aligned numeric
  columns.
