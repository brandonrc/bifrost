"""Render docs/requirements/status.csv as a standalone status page.

The CSV is the source of truth and this only presents it, so the two cannot
disagree: edit the sheet, re-run this, republish.

    python3 docs/requirements/render_status.py > /tmp/status.html
"""

import csv
import html
import subprocess
import sys

CSV = "docs/requirements/status.csv"


def kind_of(built: str) -> tuple[str, str]:
    b = built.lower()
    if b.startswith("not built"):
        return "notbuilt", "not built"
    if b.startswith("built in the extension"):
        return "external", "in the extension"
    if b.startswith("partial") or "half" in b or "provisioner side" in b:
        return "partial", "partial"
    return "built", "built"


def lane_kind(cell: str) -> str:
    c = cell.lower()
    if c in ("no tests", "n/a"):
        return "none"
    if "fail" in c:
        return "red"
    if "pinned gap" in c:
        return "pinned"
    if "skip" in c:
        return "amber"
    return "green"


def main() -> None:
    rows = list(csv.DictReader(l for l in open(CSV) if not l.startswith("# ")))
    counts: dict[str, int] = {}
    for r in rows:
        label = kind_of(r["In the source code"])[1]
        counts[label] = counts.get(label, 0) + 1
    tested = sum(1 for r in rows if (r["Automated tests"] or "0") != "0")
    total = sum(int(r["Automated tests"] or 0) for r in rows)
    sha = subprocess.run(
        ["git", "rev-parse", "--short=7", "HEAD"], capture_output=True, text=True
    ).stdout.strip()

    body = []
    for r in rows:
        cls, label = kind_of(r["In the source code"])
        where = html.escape(r["Where it lives"]).replace(", ", "<br>")
        lanes = "".join(
            f'<td class="lane {lane_kind(r[c])}">{html.escape(r[c])}</td>'
            for c in ("L2 (no cluster)", "kind lane", "grace lane")
        )
        body.append(
            f'<tr><td class="num">{r["Req"]}</td>'
            f'<td class="req"><span class="title">{html.escape(r["Requirement"])}</span> '
            f'<span class="prio p{r["Priority"].lower()}">{r["Priority"].lower()}</span> '
            f'<span class="chip {cls}">{label}</span>'
            f'<div class="built">{html.escape(r["In the source code"])}</div></td>'
            f'<td class="where"><code>{where}</code></td>'
            f'<td class="n">{r["Automated tests"]}</td>{lanes}'
            f'<td class="hand">{html.escape(r["Proven by hand"])}</td>'
            f'<td class="gap">{html.escape(r["Gap / next step"])}</td></tr>'
        )
    tiles = "".join(
        f'<div class="tile"><b>{v}</b> {k}</div>'
        for k, v in sorted(counts.items(), key=lambda kv: -kv[1])
    )
    sys.stdout.write(
        TEMPLATE.replace("__ROWS__", "\n".join(body))
        .replace("__TILES__", tiles)
        .replace("__SHA__", sha)
        .replace("__TOTAL__", str(total))
        .replace("__TESTED__", str(tested))
    )


TEMPLATE = """<title>Ray Pack Requirement Status</title>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Archivo:wght@500;700&family=Source+Serif+4:opsz,wght@8..60,400&family=IBM+Plex+Mono:wght@400&display=swap">
<style>
:root{
  --paper:#f4f4f2; --surface:#fff; --ink:#1a1d1c; --muted:#5d6462; --line:#d9dbd6; --rule:#eceee9;
  --accent:#0f6b5c; --green:#1f7a4d; --green-bg:#e2f2e8; --amber:#8a5a12; --amber-bg:#fbeed8;
  --red:#a3282a; --grey:#6a716f; --grey-bg:#e9ebe8; --purple:#6b4a97; --purple-bg:#ece5f7;
  --mono:"IBM Plex Mono",ui-monospace,Menlo,monospace;
}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){
  --paper:#121413; --surface:#191c1b; --ink:#e6e8e6; --muted:#9aa2a0; --line:#2b302e; --rule:#232725;
  --accent:#5cc0aa; --green:#63c58c; --green-bg:#13291d; --amber:#dda54e; --amber-bg:#2e2312;
  --red:#e2807f; --grey:#a2a9a7; --grey-bg:#242827; --purple:#b79be0; --purple-bg:#241d33;
}}
:root[data-theme="dark"]{
  --paper:#121413; --surface:#191c1b; --ink:#e6e8e6; --muted:#9aa2a0; --line:#2b302e; --rule:#232725;
  --accent:#5cc0aa; --green:#63c58c; --green-bg:#13291d; --amber:#dda54e; --amber-bg:#2e2312;
  --red:#e2807f; --grey:#a2a9a7; --grey-bg:#242827; --purple:#b79be0; --purple-bg:#241d33;
}
*{box-sizing:border-box}
body{margin:0;background:var(--paper);color:var(--ink);font-family:"Source Serif 4",Georgia,serif;font-size:15px;line-height:1.5}
header{padding:2rem clamp(1rem,3vw,2.5rem) 0;max-width:1500px;margin:0 auto}
h1{font-family:Archivo,system-ui,sans-serif;font-weight:700;font-size:clamp(1.6rem,3vw,2.2rem);margin:.2rem 0 .4rem;letter-spacing:-.01em;text-wrap:balance}
.eyebrow{font-family:Archivo,sans-serif;font-size:.7rem;letter-spacing:.16em;text-transform:uppercase;color:var(--muted);font-weight:700}
.lede{color:var(--muted);max-width:72ch;margin:.3rem 0 0}
.meta{display:flex;flex-wrap:wrap;gap:.4rem 1.3rem;margin:.9rem 0 0;font-family:Archivo,sans-serif;font-size:.8rem;color:var(--muted)}
.meta b{color:var(--ink);font-weight:500}
.tiles{display:flex;flex-wrap:wrap;gap:.5rem;margin:1.1rem 0 .2rem}
.tile{border:1px solid var(--line);border-radius:5px;background:var(--surface);padding:.42rem .7rem;font-family:Archivo,sans-serif;font-size:.82rem;display:flex;gap:.45rem;align-items:baseline;color:var(--muted)}
.tile b{font-size:1.05rem;color:var(--ink);font-variant-numeric:tabular-nums}
.wrap{max-width:1500px;margin:0 auto;padding:1rem clamp(1rem,3vw,2.5rem) 4rem}
.scroll{overflow-x:auto;border:1px solid var(--line);border-radius:6px;background:var(--surface)}
table{border-collapse:collapse;width:100%;min-width:1180px;font-size:.86rem}
thead th{position:sticky;top:0;background:var(--paper);font-family:Archivo,sans-serif;font-size:.7rem;letter-spacing:.07em;text-transform:uppercase;color:var(--muted);text-align:left;padding:.6rem;border-bottom:1px solid var(--line);white-space:nowrap;z-index:1}
td{padding:.62rem .6rem;border-bottom:1px solid var(--rule);vertical-align:top}
tbody tr:last-child td{border-bottom:none}
td.num{font-family:Archivo,sans-serif;font-weight:700;color:var(--accent);font-variant-numeric:tabular-nums;text-align:right;width:2.4rem}
td.req{min-width:19rem;max-width:24rem}
.title{font-family:Archivo,sans-serif;font-weight:500}
.built{color:var(--muted);font-size:.82rem;margin-top:.18rem}
td.where{max-width:19rem}
td.where code{font-family:var(--mono);font-size:.72rem;color:var(--muted);line-height:1.45}
td.n{text-align:right;font-variant-numeric:tabular-nums;font-family:Archivo,sans-serif;width:3.2rem}
td.lane{white-space:nowrap;font-family:Archivo,sans-serif;font-size:.78rem}
td.lane.green{color:var(--green)}
td.lane.amber{color:var(--amber)}
td.lane.red{color:var(--red);font-weight:700}
td.lane.none,td.lane.pinned{color:var(--grey)}
td.hand,td.gap{max-width:24rem}
td.gap{color:var(--muted)}
.chip,.prio{font-family:Archivo,sans-serif;font-size:.66rem;letter-spacing:.06em;text-transform:uppercase;font-weight:700;padding:.1em .45em;border-radius:3px;white-space:nowrap}
.chip.built{background:var(--green-bg);color:var(--green)}
.chip.partial{background:var(--amber-bg);color:var(--amber)}
.chip.notbuilt{background:var(--grey-bg);color:var(--grey)}
.chip.external{background:var(--purple-bg);color:var(--purple)}
.prio{background:transparent;border:1px solid var(--line);color:var(--muted)}
.prio.pcritical{color:var(--red);border-color:var(--red)}
.legend{margin:1rem 0 0;color:var(--muted);font-size:.86rem;max-width:92ch}
.legend p{margin:.5rem 0}
.legend code{font-family:var(--mono);font-size:.78rem}
</style>
<header>
  <div class="eyebrow">Bifrost · Ray Software Pack</div>
  <h1>Requirement status</h1>
  <p class="lede">The eighteen rows, what is in the source, and what an automated test actually proves about each one. Two things are kept apart on purpose: a row can be built and still be untested, and a row can pass every lane and still be unproven where it matters.</p>
  <div class="meta"><span>main <b>__SHA__</b></span><span>curated <b>2026-09-04</b></span><span>suite <b>__TOTAL__ tests</b></span><span>rows with tests <b>__TESTED__ of 18</b></span></div>
  <div class="tiles">__TILES__</div>
</header>
<div class="wrap">
  <div class="scroll">
  <table>
    <thead><tr>
      <th>#</th><th>Requirement</th><th>Where it lives</th><th>Tests</th>
      <th>L2 no cluster</th><th>kind lane</th><th>grace lane</th>
      <th>Proven by hand</th><th>Gap / next step</th>
    </tr></thead>
    <tbody>__ROWS__</tbody>
  </table>
  </div>
  <div class="legend">
    <p><b>Lanes.</b> L2 runs on every push with no cluster and its numbers come from the committed matrix on main. The kind lane runs four shards on a throwaway cluster, last green on run 33891270587. The grace figures are the last full run against the live deployment, which the in-cluster nightly reproduces. Skips are normal and recorded: a test skips when its target lacks the capability it needs, which is why a row can show more passes on a real cluster than on L2.</p>
    <p><b>The rows no lane can speak for.</b> Nine and eleven live in the JupyterLab extension, a separate repository with its own suites; nine now has live evidence against grace, eleven has none. Sixteen is deferred by ruling and pinned by a test that fails on purpose. Seventeen has only a compile-time guard that the provisioner interface stays free of Kubernetes types.</p>
    <p>Generated from <code>docs/requirements/status.csv</code> by <code>docs/requirements/render_status.py</code>. Lane numbers regenerate with <code>make report</code> and the lane artifacts; the curated columns do not, so date any edit.</p>
  </div>
</div>
"""

if __name__ == "__main__":
    main()
