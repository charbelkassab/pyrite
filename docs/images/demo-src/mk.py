import html, os, re

W, H = 960, 540
OUT = os.path.dirname(os.path.abspath(__file__))

CSS = """
*{margin:0;padding:0;box-sizing:border-box}
html,body{width:%dpx;height:%dpx}
body{background:#0b0d11;font-family:"Hack","Liberation Mono",monospace;
     font-size:15px;line-height:1.5;color:#c9c5bd;padding:0;overflow:hidden}
.bar{height:34px;background:#15181e;border-bottom:1px solid #23272f;
     display:flex;align-items:center;padding:0 14px;gap:8px}
.dot{width:11px;height:11px;border-radius:50%%}
.t{margin-left:10px;font-size:12.5px;color:#6d6a63;letter-spacing:.3px}
.body{padding:16px 20px}
.cmd{color:#e8e4dc;margin-bottom:12px;font-size:15.5px}
.cmd .p{color:#d9a441;font-weight:700}
pre{white-space:pre;font-family:inherit;font-size:15px;line-height:1.5}
.g{color:#5fbf7f}.r{color:#e2705c}.a{color:#d9a441}.d{color:#6d6a63}
.w{color:#f0ece4;font-weight:700}
.cap{position:absolute;left:20px;bottom:16px;font-size:14px;color:#7d7970}
.cap b{color:#d9a441;font-weight:700}
""" % (W, H)

def esc(s): return html.escape(s)

def paint(text, rules):
    out = []
    for line in text.split("\n"):
        e = esc(line)
        for pat, cls in rules:
            e = re.sub(pat, lambda m: '<span class="%s">%s</span>' % (cls, m.group(0)), e)
        out.append(e)
    return "\n".join(out)

NUM_RULES = [
    (r'-\d[\d,.]*%', 'r'),
    (r'STOP', 'r'),
]

def frame(n, cmd, body, caption, rules=NUM_RULES):
    painted = paint(body, rules)
    doc = f"""<!doctype html><html><head><meta charset="utf-8"><style>{CSS}</style></head><body>
<div class="bar">
  <span class="dot" style="background:#e2705c"></span>
  <span class="dot" style="background:#d9a441"></span>
  <span class="dot" style="background:#5fbf7f"></span>
  <span class="t">pyrite</span>
</div>
<div class="body">
  <div class="cmd"><span class="p">$</span> {esc(cmd)}</div>
  <pre>{painted}</pre>
</div>
<div class="cap">{caption}</div>
</body></html>"""
    open(os.path.join(OUT, f"f{n}.html"), "w").write(doc)

def read(p, limit=None):
    lines = open(os.path.join(OUT, "..", p)).read().rstrip("\n").split("\n")
    return "\n".join(lines[:limit] if limit else lines)

# 1 — the result that looks good
s1 = read("f1.txt", 17)
frame(1, "pyrite run --example golden-cross --from 2018-01-02 --to 2023-12-29",
      s1, "A classic strategy. <b>+74.5%</b>, Sharpe 0.92. Looks like an edge.")

# 2 — the critique
frame(2, "pyrite run --example golden-cross --from 2018-01-02 --to 2023-12-29",
      read("s1b.txt", 13),
      "Every run also reports <b>the case against itself</b>.")

# 3 — the sweep
frame(3, "pyrite sweep --example golden-cross --from 2015-01-05 --to 2023-12-29",
      read("s2.txt", 13),
      "160 combinations. A broad plateau &mdash; but <b>81% chance of overfitting</b>.")

# 4 — walk-forward
frame(4, "pyrite walkforward --example golden-cross --train 500 --test 150",
      read("s3.txt", 11),
      "Chosen on one period, judged on the next: <b>it was fitted all along</b>.")
print("wrote 4 frames")
