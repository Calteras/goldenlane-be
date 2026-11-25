#!/usr/bin/env python3
"""Generates the SVG diagrams used by the documents in docs/ into docs/images/.

    python3 scripts/gen-diagrams.py

No dependencies. Each diagram is described in plain Python near the bottom of this file; edit it
and run the script again. The SVGs are committed, so only authors of diagrams need this script.
"""
import os
from xml.sax.saxutils import escape

OUT = os.path.join(os.path.dirname(__file__), "..", "docs", "images")
FONT = "-apple-system, 'Segoe UI', Helvetica, Arial, sans-serif"
MONO = "ui-monospace, 'SF Mono', Menlo, Consolas, monospace"
INK = "#1e293b"
MUTED = "#64748b"
LINE = "#475569"

# name: (fill, stroke, text)
PAL = {
    "pos": ("#dbeafe", "#2563eb", "#1e3a8a"),
    "api": ("#dcfce7", "#16a34a", "#14532d"),
    "customer": ("#ffedd5", "#ea580c", "#7c2d12"),
    "vendor": ("#ede9fe", "#7c3aed", "#4c1d95"),
    "gateway": ("#e2e8f0", "#475569", "#1e293b"),
    "firebase": ("#fef9c3", "#ca8a04", "#713f12"),
    "operator": ("#fce7f3", "#db2777", "#831843"),
    "ok": ("#dcfce7", "#16a34a", "#14532d"),
    "bad": ("#fee2e2", "#dc2626", "#7f1d1d"),
    "warn": ("#fef3c7", "#d97706", "#78350f"),
    "plain": ("#f8fafc", "#94a3b8", "#1e293b"),
}


def tw(text, size=13, mono=False):
    """Approximate rendered width of a text in px."""
    if mono:
        return len(text) * size * 0.61
    narrow = set("iljtfr.,:;'|!()[]{} ")
    wide = set("mwMW@%")
    w = 0.0
    for ch in text:
        if ch in narrow:
            w += 0.32
        elif ch in wide:
            w += 0.88
        elif ch.isupper():
            w += 0.67
        else:
            w += 0.55
    return w * size * 1.04


def wrap(text, maxw, size=13, mono=False):
    """Wraps text (explicit newlines kept) to a pixel width."""
    lines = []
    for para in text.split("\n"):
        words = para.split(" ")
        cur = ""
        for word in words:
            trial = (cur + " " + word).strip()
            if cur and tw(trial, size, mono) > maxw:
                lines.append(cur)
                cur = word
            else:
                cur = trial
        lines.append(cur)
    return lines


class Box:
    def __init__(self, x, y, w, h):
        self.x, self.y, self.w, self.h = x, y, w, h

    l = property(lambda s: (s.x, s.y + s.h / 2))
    r = property(lambda s: (s.x + s.w, s.y + s.h / 2))
    t = property(lambda s: (s.x + s.w / 2, s.y))
    b = property(lambda s: (s.x + s.w / 2, s.y + s.h))
    cx = property(lambda s: s.x + s.w / 2)
    cy = property(lambda s: s.y + s.h / 2)

    def at(self, side, frac=0.5):
        if side == "l":
            return (self.x, self.y + self.h * frac)
        if side == "r":
            return (self.x + self.w, self.y + self.h * frac)
        if side == "t":
            return (self.x + self.w * frac, self.y)
        return (self.x + self.w * frac, self.y + self.h)


class D:
    def __init__(self, title=None, desc=None):
        self.layers = [[], [], [], []]  # lines, shapes, arrows, text
        self.markers = {}
        self.minx = self.miny = 1e9
        self.maxx = self.maxy = -1e9
        self.title = title
        self.desc = desc

    def grow(self, x1, y1, x2, y2):
        self.minx, self.miny = min(self.minx, x1), min(self.miny, y1)
        self.maxx, self.maxy = max(self.maxx, x2), max(self.maxy, y2)

    def marker(self, color):
        if color not in self.markers:
            self.markers[color] = "a%d" % len(self.markers)
        return self.markers[color]

    # ------------------------------------------------------------ primitives
    def text(self, x, y, s, size=13, color=INK, anchor="middle", weight="normal", mono=False, halo=False, italic=False):
        fam = MONO if mono else FONT
        extra = ' stroke="#ffffff" stroke-width="5" stroke-linejoin="round" paint-order="stroke"' if halo else ""
        style = ' font-style="italic"' if italic else ""
        self.layers[3].append(
            f'<text x="{x:.1f}" y="{y:.1f}" font-family="{fam}" font-size="{size}" fill="{color}" '
            f'text-anchor="{anchor}" font-weight="{weight}"{style}{extra}>{escape(s)}</text>'
        )
        w = tw(s, size, mono)
        x0 = x - w / 2 if anchor == "middle" else (x if anchor == "start" else x - w)
        self.grow(x0, y - size, x0 + w, y + 4)

    def lines(self, cx, y, lines, size=13, color=INK, anchor="middle", lh=17, weight="normal", mono=False, halo=False):
        for i, ln in enumerate(lines):
            self.text(cx, y + i * lh, ln, size, color, anchor, weight, mono, halo)
        return y + len(lines) * lh

    def rect(self, x, y, w, h, color="plain", rx=10, dashed=False, sw=1.5, fill=None, stroke=None):
        f, s, _ = PAL[color]
        f = fill or f
        s = stroke or s
        d = ' stroke-dasharray="6 4"' if dashed else ""
        self.layers[1].append(
            f'<rect x="{x:.1f}" y="{y:.1f}" width="{w:.1f}" height="{h:.1f}" rx="{rx}" fill="{f}" stroke="{s}" stroke-width="{sw}"{d}/>'
        )
        self.grow(x, y, x + w, y + h)

    def box(self, x, y, w, h, lines, color="plain", title=None, mono=False, size=13, dashed=False, align="middle"):
        """A rounded box with an optional bold title and text lines."""
        self.rect(x, y, w, h, color, dashed=dashed)
        _, _, tc = PAL[color]
        n = len(lines) + (1 if title else 0)
        lh = size + 5
        y0 = y + h / 2 - (n * lh) / 2 + size
        if title:
            self.text(x + w / 2, y0, title, size + 1, tc, weight="bold")
            y0 += lh
        tx = x + w / 2 if align == "middle" else x + 14
        self.lines(tx, y0, lines, size, INK if not title else INK, align if align != "middle" else "middle", lh, mono=mono)
        return Box(x, y, w, h)

    def line(self, pts, color=LINE, dashed=False, width=1.8, arrow=True, both=False):
        m = self.marker(color)
        d = " ".join(("M" if i == 0 else "L") + f"{x:.1f},{y:.1f}" for i, (x, y) in enumerate(pts))
        dash = ' stroke-dasharray="7 5"' if dashed else ""
        ms = f' marker-end="url(#{m})"' if arrow else ""
        mb = f' marker-start="url(#{m})"' if both else ""
        self.layers[2].append(
            f'<path d="{d}" fill="none" stroke="{color}" stroke-width="{width}"{dash}{ms}{mb} stroke-linejoin="round"/>'
        )
        for x, y in pts:
            self.grow(x, y, x, y)

    def label(self, x, y, lines, size=12, color=MUTED, mono=False, anchor="middle"):
        if isinstance(lines, str):
            lines = lines.split("\n")
        self.lines(x, y, lines, size, color, anchor, size + 4, mono=mono, halo=True)

    # --------------------------------------------------------------- output
    def render(self, margin=24):
        title_h = 0
        if self.title:
            title_h = 34
        x0, y0 = self.minx - margin, self.miny - margin - title_h
        w = self.maxx - self.minx + 2 * margin
        h = self.maxy - self.miny + 2 * margin + title_h
        w = max(w, tw(self.title or "", 16) + 2 * margin)
        defs = "".join(
            f'<marker id="{i}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse">'
            f'<path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>'
            for c, i in self.markers.items()
        )
        head = ""
        if self.title:
            head = (
                f'<text x="{x0 + margin:.1f}" y="{y0 + margin + 8:.1f}" font-family="{FONT}" font-size="16" '
                f'font-weight="bold" fill="{INK}">{escape(self.title)}</text>'
            )
        body = "\n".join("\n".join(layer) for layer in self.layers)
        desc = f"<desc>{escape(self.desc)}</desc>" if self.desc else ""
        ttl = f"<title>{escape(self.title)}</title>" if self.title else ""
        return (
            f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{x0:.0f} {y0:.0f} {w:.0f} {h:.0f}" '
            f'width="{w:.0f}" height="{h:.0f}" role="img">{ttl}{desc}<defs>{defs}</defs>'
            f'<rect x="{x0:.0f}" y="{y0:.0f}" width="{w:.0f}" height="{h:.0f}" fill="#ffffff"/>\n{head}\n{body}\n</svg>\n'
        )


# ====================================================================== sequence
def sequence(title, actors, steps, gap=240, desc=None):
    """actors: list of (key, title, subtitle, color). steps: tuples, see below.

    ("msg", a, b, label, dashed=False, num=None)   an arrow from lane a to lane b
    ("note", span, text, color="plain")             a box over a lane, or over (a, b)
    ("section", text)                               a band across all lanes
    ("gap", px)                                     vertical space
    """
    d = D(title, desc)
    head_w = gap - 28
    xs = {}
    for i, (key, *_r) in enumerate(actors):
        xs[key] = head_w / 2 + i * gap
    color_of = {a[0]: a[3] for a in actors}
    head_h = 54
    for key, ttl, sub, col in actors:
        d.rect(xs[key] - head_w / 2, 0, head_w, head_h, col, rx=12)
        _, _, tc = PAL[col]
        if sub:
            d.text(xs[key], 22, ttl, 14, tc, weight="bold")
            d.text(xs[key], 40, sub, 11.5, MUTED)
        else:
            d.text(xs[key], 32, ttl, 14, tc, weight="bold")
    y = head_h + 30
    left = xs[actors[0][0]] - head_w / 2
    right = xs[actors[-1][0]] + head_w / 2
    for st in steps:
        kind = st[0]
        if kind == "gap":
            y += st[1]
        elif kind == "section":
            d.rect(left, y - 4, right - left, 26, "plain", rx=6, fill="#f1f5f9", stroke="#cbd5e1", sw=1)
            d.text((left + right) / 2, y + 14, st[1], 12.5, MUTED, weight="bold")
            y += 44
        elif kind == "msg":
            _, a, b, label = st[:4]
            dashed = st[4] if len(st) > 4 else False
            num = st[5] if len(st) > 5 else None
            x1, x2 = xs[a], xs[b]
            span = abs(x2 - x1)
            lines = wrap(label, max(span - 40, 120), 12.5)
            ly = y
            lh = 16
            col = LINE
            if x1 == x2:
                raise ValueError("self messages are not supported: use a note")
            # label above the arrow
            mid = (x1 + x2) / 2
            ty = ly + 12
            for i, ln in enumerate(lines):
                d.text(mid, ty + i * lh, ln, 12.5, INK, halo=True)
            if num is not None:
                first = lines[0]
                nx = mid - tw(first, 12.5) / 2 - 14
                d.rect(nx - 8, ty - 11, 16, 16, "plain", rx=8, fill="#1e293b", stroke="#1e293b", sw=1)
                d.text(nx, ty + 1, str(num), 10.5, "#ffffff", weight="bold")
            ay = ly + 12 + (len(lines) - 1) * lh + 12
            off = 7
            sx = x1 + (off if x2 > x1 else -off)
            ex = x2 - (off if x2 > x1 else -off)
            d.line([(sx, ay), (ex, ay)], col, dashed)
            y = ay + 22
        elif kind == "note":
            span, text = st[1], st[2]
            color = st[3] if len(st) > 3 else color_of.get(span if isinstance(span, str) else span[0], "plain")
            if isinstance(span, str):
                cx = xs[span]
                maxw = gap * 1.75
                lines = wrap(text, maxw - 28, 12.5)
                w = max(tw(l, 12.5) for l in lines) + 28
                w = max(w, 120)
                x = cx - w / 2
            else:
                xa, xb = xs[span[0]], xs[span[1]]
                x = min(xa, xb) - head_w / 2 + 6
                w = abs(xb - xa) + head_w - 12
                lines = wrap(text, w - 28, 12.5)
                cx = x + w / 2
            h = len(lines) * 16 + 16
            d.rect(x, y, w, h, color, rx=8, sw=1.3)
            _, _, tc = PAL[color]
            d.lines(cx, y + 20, lines, 12.5, INK, lh=16)
            y += h + 18
    bottom = y
    for key, *_r in actors:
        d.layers[0].append(
            f'<line x1="{xs[key]:.1f}" y1="{head_h}" x2="{xs[key]:.1f}" y2="{bottom:.1f}" stroke="#cbd5e1" stroke-width="1.5" stroke-dasharray="5 5"/>'
        )
    d.grow(left, 0, right, bottom)
    return d


# ===================================================================== step list
BADGE = {
    "GET": ("#dbeafe", "#1d4ed8"), "POST": ("#dcfce7", "#15803d"), "PUT": ("#fef3c7", "#b45309"),
    "PATCH": ("#fef3c7", "#b45309"), "DELETE": ("#fee2e2", "#b91c1c"), "SOCKET": ("#ede9fe", "#6d28d9"),
    "APP": ("#ffedd5", "#c2410c"), "AUTH": ("#fef9c3", "#854d0e"), "PUSH": ("#ede9fe", "#6d28d9"),
}


def steplist(title, groups, width=900, badge_w=76, desc=None):
    """groups: list of (heading or None, [(n or None, badge, main, note), ...])."""
    d = D(title, desc)
    y = 0
    for heading, rows in groups:
        if heading:
            d.rect(0, y, width, 28, "plain", rx=6, fill="#f1f5f9", stroke="#cbd5e1", sw=1)
            d.text(14, y + 19, heading, 13, MUTED, anchor="start", weight="bold")
            y += 40
        for n, badge, main, note in rows:
            h = 40
            d.rect(0, y, width, h, "plain", rx=8, fill="#ffffff", stroke="#e2e8f0", sw=1)
            if n is not None:
                d.rect(10, y + 9, 22, 22, "plain", rx=11, fill="#1e293b", stroke="#1e293b", sw=1)
                d.text(21, y + 24.5, str(n), 11.5, "#ffffff", weight="bold")
            bx = 44
            fill, col = BADGE.get(badge.split()[0].upper(), ("#e2e8f0", "#334155"))
            bw = max(badge_w, tw(badge, 11.5, True) + 16)
            d.rect(bx, y + 9, bw, 22, "plain", rx=5, fill=fill, stroke=fill, sw=1)
            d.text(bx + bw / 2, y + 24.5, badge, 11.5, col, weight="bold", mono=True)
            mx = bx + max(badge_w, 0) + 18
            mono = main.startswith("/") or main.startswith("streetfood") or "/v1/" in main
            d.text(mx, y + 25, main, 12.5, INK, anchor="start", mono=mono)
            if note:
                d.text(width - 14, y + 25, note, 12, MUTED, anchor="end")
            y += h + 6
        y += 8
    return d


# ====================================================================== helpers
def chain(d, boxes, color_gap=0, label_fn=None):
    for i in range(len(boxes) - 1):
        a, b = boxes[i], boxes[i + 1]
        d.line([a.r, b.l])


def build(name, d):
    os.makedirs(OUT, exist_ok=True)
    path = os.path.join(OUT, name + ".svg")
    with open(path, "w") as f:
        f.write(d.render())
    print("wrote", os.path.relpath(path))


A = lambda k, t, s, c: (k, t, s, c)

# ======================================================================= diagrams


def architecture():
    d = D("The cast and the five channels between them")
    # customer side top
    staff = d.box(0, 0, 250, 56, ["Cashier tablet, vendor phone"], "vendor", "Staff apps (Ansilum)")
    cust = d.box(520, 0, 250, 56, ["Goldenlane (Flutter)"], "customer", "Customer app")
    pos = d.box(0, 120, 250, 230, ["NestJS + PostgreSQL", "outlets, hours, menus, prices,", "the working day, GPS trails,", "orders and sales"], "pos", "POS  (ansilum-be)")
    api = d.box(520, 120, 250, 230, ["Go + its own PostgreSQL", "accounts, search, orders and", "payments, push, reviews,", "the fee ledger"], "api", "Customer API")
    gw = d.box(520, 440, 250, 64, ["the merchant's own account"], "gateway", "Xendit / Midtrans")
    d.line([staff.b, (staff.cx, pos.y)])
    d.line([cust.b, (cust.cx, api.y)])
    x1, x2 = pos.x + pos.w, api.x
    ys = [160, 215, 270, 325]
    labels = [
        ("1. events: what changed", "POS → API", True),
        ("2. reads: outlets, menus", "API → POS", False),
        ("3. orders: price, place, cancel, status", "API → POS", False),
        ("4. fees: reserve, report", "POS → API", True),
    ]
    for y, (lab, direction, fwd) in zip(ys, labels):
        pts = [(x1 + 6, y), (x2 - 6, y)] if fwd else [(x2 - 6, y), (x1 + 6, y)]
        d.line(pts)
        d.label((x1 + x2) / 2, y - 7, lab, 12, INK)
    d.line([api.b, (api.cx, gw.y)], both=True)
    d.label(api.cx + 14, 410, ["5. charge, refund, status (API → gateway)", "callbacks (gateway → API)"], 12, INK, anchor="start")
    d.label((x1 + x2) / 2, 395, "Never: the customer app calls the POS, or the\nvendor's phone calls the customer API", 11.5, "#b91c1c")
    return d


def link_creation():
    return sequence(
        "Creating the link between a business and the platform",
        [A("o", "Business owner", "Ansilum dashboard", "vendor"), A("p", "POS", "ansilum-be", "pos"),
         A("op", "Operator", "platform administrator", "operator"), A("a", "Customer API", "this repository", "api")],
        [
            ("msg", "o", "p", "POST /link  {platformUrl}", False, 1),
            ("note", "p", "makes keyId (sfk_…) and secret (sfs_…); stores the secret encrypted"),
            ("msg", "p", "o", "keyId + secret (shown ONCE)", True),
            ("msg", "o", "p", "publish the outlets", False, 2),
            ("section", "The owner hands the credentials to the operator (securely, outside the system)"),
            ("msg", "o", "op", "keyId, secret, tenant id", True, 3),
            ("msg", "op", "a", "POST /v1/admin/merchants  {name, posTenantId, keyId, secret, posBaseUrl}", False, 4),
            ("msg", "a", "p", "signed GET /outlets  (does the secret work?)", False, 5),
            ("msg", "p", "a", "the outlets", True),
            ("note", "a", "stores the secret encrypted; loads outlets and menus at once"),
        ],
        gap=250,
        desc="Sequence of creating the link: owner, POS, operator, customer API.",
    )


def push_pipeline():
    d = D("Push: how a change travels from the POS to the customer API")
    w, h, x = 430, 62, 0
    a = d.box(x, 0, w, h, ["menu edit, vendor opens the day,", "a position, an order status…"], "pos", "Something changes in the POS")
    b = d.box(x, 100, w, h, ["written in the SAME database transaction", "as the change"], "pos", "Outbox row")
    c = d.box(x, 200, w, h, ["leases up to 20 waiting events per link"], "pos", "POS worker (every 5 s)")
    e = d.box(x, 320, w, 82, ["verifies the signature", "stores each event in the inbox", "answers 204"], "api", "Customer API receives")
    f = d.box(x, 440, w, h, ["applies the stored events, newest version wins; sleeps when none are due"], "api", "Worker (wakes after the commit)")
    d.line([a.b, b.t])
    d.line([b.b, c.t])
    d.line([c.b, e.t])
    d.line([e.b, f.t])
    d.label(x + w / 2 + 12, 296, "POST {platformUrl}/v1/integrations/pos/events  (signed batch)", 12, INK, anchor="start")
    d.label(x + w + 20, 135, ["commit, or nothing at all:", "no \"changed but never announced\""], 12, MUTED, anchor="start")
    d.label(x + w + 20, 360, ["a duplicate event id is ignored", "the answer is 204 once STORED,", "not once applied"], 12, MUTED, anchor="start")
    d.label(x + w + 20, 470, "a crash between storing and applying loses nothing", 12, MUTED, anchor="start")
    return d


def gps_reading():
    return sequence(
        "A GPS reading, step by step",
        [A("ph", "Vendor phone", "Ansilum app", "vendor"), A("p", "POS", "tracking rules", "pos"), A("a", "Customer API", "", "api")],
        [
            ("msg", "ph", "p", "POST location/pings: a batch of up to 100, every 15 to 120 s, queued while offline", False, 1),
            ("note", "p", "1. judge every reading on its own (one bad reading never discards the batch)\n2. store all accepted readings in the trail (owner's reports)\n3. current position = newest trusted reading\n4. decide whether to publish it (the throttle: 15 s and 20 m)\n5. answer the phone how often to send next"),
            ("msg", "p", "ph", "accepted, flagged, rejected, nextIntervalSeconds", True, 2),
            ("msg", "p", "a", "event outlet.location.updated, only if the throttle allows (signed batch)", False, 3),
            ("note", "a", "stores the position if it is newer than the current one, then recomputes where customers find the outlet"),
        ],
        gap=330,
    )


def position_rules():
    d = D("What the customer API does with the events")
    ev = [
        ("outlet.session.changed", "the day opened or closed,\nsharing, spot or anchor"),
        ("outlet.location.updated", "a new position"),
        ("outlet.location.cleared", "the day ended or the vendor\nhid itself"),
    ]
    wr = [
        ["outlets.session_* columns", "(active, id, sharing, spot / anchor)"],
        ["outlets.cur_* columns", "only if recordedAt is newer than the current", "position AND the last \"cleared\""],
        ["clears cur_*", "and remembers when"],
    ]
    ys = [0, 110, 235]
    left, right = [], []
    for (name, sub), y, lines in zip(ev, ys, wr):
        h = 84 if len(lines) == 3 else 70
        b1 = d.box(0, y, 260, h, sub.split("\n"), "pos", name, mono=False)
        b2 = d.box(360, y, 320, h, lines, "api")
        d.line([b1.r, b2.l])
        left.append(b1)
        right.append(b2)
    last = d.box(110, 380, 480, 78, ["outlets.pos_lat / pos_lng / pos_mode  (indexed)", "one function: Outlet.DerivePosition"], "api", "Recompute: where do customers find it?")
    bus = 730
    for r in right:
        d.line([r.r, (bus, r.cy)], arrow=False)
    d.line([(bus, right[0].cy), (bus, last.cy), last.r], arrow=True)
    return d


def position_timeline():
    d = D("One position's journey, with time")
    rows = [
        ("t = 0.0 s", "vendor", "The vendor's phone sends a reading; the POS judges and stores it"),
        ("t = 0.0 s", "pos", "POS: at least 15 s since the last publication and moved 20 m or more: it writes the outbox event"),
        ("t ≤ 5 s", "pos", "The POS worker picks it up and POSTs the signed batch"),
        ("t + ~50 ms", "api", "The customer API verifies it, stores it in the inbox and answers 204"),
        ("t ≤ 1 s", "api", "The customer API's worker applies it: pos_lat and pos_lng are updated"),
        ("next request", "customer", "GET /v1/outlets/nearby shows the new position"),
    ]
    y = 0
    prev = None
    for t, who, text in rows:
        f, s, tc = PAL[who]
        d.rect(0, y, 120, 34, who, rx=17)
        d.text(60, y + 22, t, 12.5, tc, weight="bold")
        d.rect(140, y, 700, 34, "plain", rx=8, fill="#ffffff", stroke="#e2e8f0", sw=1)
        d.text(154, y + 22, text, 12.5, INK, anchor="start")
        d.layers[1].append(f'<circle cx="130" cy="{y + 17}" r="0"/>')
        if prev is not None:
            d.line([(60, prev + 34), (60, y)], arrow=True)
        prev = y
        y += 58
    d.label(420, y + 4, "A customer sees a moving cart a few seconds behind the vendor: updated at most every 15 s, and only when the cart moved.", 12, MUTED)
    return d


def order_pickup():
    return sequence(
        "An order paid at pickup",
        [A("c", "Customer", "Goldenlane", "customer"), A("a", "Customer API", "", "api"), A("p", "POS", "ansilum-be", "pos"), A("k", "Cashier", "Ansilum", "vendor")],
        [
            ("msg", "c", "a", "POST /v1/checkouts  (Idempotency-Key)", False, 1),
            ("note", "a", "check the cart against the stored menu (friendly early errors); store the order as CREATED (unique per customer + key)"),
            ("msg", "a", "p", "signed POST /orders  {platformOrderId, items, expectedTotalMinor, payment}", False, 2),
            ("note", "p", "price it (the only price authority), check the outlet is open, store it as PENDING_ACCEPTANCE"),
            ("msg", "p", "k", "socket + push: new order", False, 3),
            ("msg", "p", "a", "pricing, acceptBy", True),
            ("msg", "a", "c", "order, PENDING_ACCEPTANCE", True, 4),
            ("section", "The cashier answers before acceptBy (5 minutes by default)"),
            ("msg", "k", "p", "accept", False, 5),
            ("note", "p", "status ACCEPTED, version 2"),
            ("msg", "p", "a", "event order.status.changed (outbox)", False, 6),
            ("note", "a", "apply if the version is newer; queue ONE push per (order, status); wake the open streams"),
            ("msg", "a", "c", "push + live stream: ACCEPTED", True, 7),
            ("note", ("p", "c"), "Then start (stock is consumed), ready and complete (the sale is recorded) reach the customer the same way, steps 5 to 7."),
        ],
        gap=250,
    )


def online_payment():
    return sequence(
        "An order paid online",
        [A("c", "Customer", "Goldenlane", "customer"), A("a", "Customer API", "", "api"), A("p", "POS", "ansilum-be", "pos"), A("g", "Gateway", "merchant's own account", "gateway")],
        [
            ("msg", "c", "a", "POST /v1/checkouts  payment ONLINE", False, 1),
            ("msg", "a", "p", "signed POST /orders/quote", False, 2),
            ("msg", "p", "a", "pricing (stores nothing)", True),
            ("note", "a", "store the order UNPAID, 15-minute deadline"),
            ("msg", "a", "g", "create an invoice for total + 2.5%", False, 3),
            ("msg", "g", "a", "payment page", True),
            ("msg", "a", "c", "payUrl", True, 4),
            ("msg", "c", "g", "the customer pays on the gateway's page", False, 5),
            ("msg", "g", "a", "callback: authenticated, but its content is IGNORED", False, 6),
            ("msg", "a", "g", "ask the gateway for the payment's real state", False, 7),
            ("msg", "g", "a", "PAID", True),
            ("note", "a", "order UNPAID becomes CREATED"),
            ("msg", "a", "p", "signed POST /orders  payment PREPAID, reference", False, 8),
            ("note", ("p", "c"), "From here as in an order paid at pickup, marked prepaid: accept, prepare, ready, complete. A refund is automatic if the order will not happen."),
        ],
        gap=250,
    )


def fee_collection():
    return sequence(
        "How the platform's fees are collected",
        [A("a", "Customer API", "the fee ledger", "api"), A("p", "POS", "subscription renewal", "pos"), A("m", "Saved card", "Midtrans", "gateway")],
        [
            ("note", "a", "order completed: online order = customer fee + merchant fee; pay-at-pickup order = merchant fee (rate fixed when ordered)"),
            ("section", "A renewal is due: the POS, about 24 h before the period ends, with a card on file"),
            ("msg", "p", "a", "signed POST /v1/integrations/pos/fees/reserve  {billingRef}", False, 1),
            ("note", "a", "sets the open fees aside for that billing period (the same period always gets the same answer)"),
            ("msg", "a", "p", "lines and total", True, 2),
            ("msg", "p", "m", "charge the saved card ONCE: plan price + fees", False, 3),
            ("note", "p", "payment recorded; in the SAME database transaction the POS writes a billing.fees.settled event"),
            ("msg", "p", "a", "event billing.fees.settled (worker)", False, 4),
            ("note", "a", "marks exactly those entries collected, against the invoice number"),
        ],
        gap=330,
    )


def sign_in():
    return sequence(
        "Sign-in: Firebase in the app, the ID token on every call",
        [A("u", "Customer", "", "customer"), A("app", "Goldenlane app", "", "customer"), A("f", "Firebase", "Authentication", "firebase"), A("a", "Customer API", "", "api")],
        [
            ("msg", "u", "app", "phone number", False, 1),
            ("msg", "app", "f", "verifyPhoneNumber", False),
            ("msg", "f", "u", "SMS code", True, 2),
            ("msg", "u", "app", "enters the code", False),
            ("msg", "app", "f", "signInWithCredential", False, 3),
            ("msg", "f", "app", "user + ID token (valid 1 hour)", True),
            ("msg", "app", "a", "GET /v1/me  Authorization: Bearer <ID token>", False, 4),
            ("note", "a", "verifies the token against the Firebase project; the first valid token creates the account"),
            ("msg", "a", "app", "the customer", True),
        ],
        gap=235,
    )


def customer_journey():
    return steplist(
        "A full customer journey",
        [
            ("Before ordering", [
                (1, "AUTH", "Sign in with Firebase (phone)", "Firebase"),
                (2, "GET", "/v1/me", "creates the account"),
                (3, "POST", "/v1/devices", "register the FCM token"),
            ]),
            ("Finding food", [
                (4, "GET", "/v1/outlets/nearby?lat&lng", "map and list, poll every 15 s"),
                (5, "GET", "/v1/outlets/{id}  and  /v1/outlets/{id}/menu", "vendor page"),
            ]),
            ("Ordering", [
                (6, "POST", "/v1/checkouts   (Idempotency-Key)", "PAY_AT_PICKUP or ONLINE"),
                (None, "APP", "ONLINE: open payment.payUrl, order is UNPAID until paid", "section 8"),
            ]),
            ("After ordering", [
                (7, "GET", "/v1/orders/{id}/stream   (or push)", "ACCEPTED, PREPARING, READY, COMPLETED"),
                (8, "GET", "/v1/outlets/{id}   every 15 s", "see where the vendor is"),
                (9, "POST", "/v1/orders/{id}/review", "after COMPLETED"),
            ]),
        ],
        width=900,
    )


def state_flow(title, top, extra, desc=None):
    """top: list of (label, color); extra: list of (from_index, label, color, arrow label, note)."""
    d = D(title, desc)
    bw, bh, gapx = 150, 48, 48
    boxes = []
    for i, (lab, col, *_r) in enumerate(top):
        b = d.box(i * (bw + gapx), 0, bw, bh, [lab], col, mono=False, size=12.5)
        boxes.append(b)
    return d, boxes, bw, bh, gapx


def vendor_order_flow():
    d = D("An online order at the register")
    bw, bh, gx = 150, 48, 70
    names = [("PENDING_ACCEPTANCE", "warn"), ("ACCEPTED", "pos"), ("PREPARING", "pos"), ("READY", "pos"), ("COMPLETED", "ok")]
    acts = ["accept", "start", "ready", "complete"]
    boxes = [d.box(i * (bw + gx), 0, bw, bh, [n], c, size=12) for i, (n, c) in enumerate(names)]
    for i, a in enumerate(acts):
        d.line([boxes[i].r, boxes[i + 1].l])
        d.label((boxes[i].r[0] + boxes[i + 1].l[0]) / 2, -9, a, 12, INK, mono=True)
    p, ac = boxes[0], boxes[1]
    rej = d.box(p.x + p.w * 0.1 - 50, 150, 100, bh, ["REJECTED"], "bad", size=12)
    exp = d.box(p.x + p.w * 0.9 - 50, 150, 100, bh, ["EXPIRED"], "bad", size=12)
    can = d.box(ac.cx - 50, 150, 100, bh, ["CANCELLED"], "bad", size=12)
    d.line([p.at("b", 0.1), (rej.cx, 150)])
    d.label(rej.cx - 8, 112, "reject", 12, INK, mono=True, anchor="end")
    d.line([p.at("b", 0.9), (exp.cx, 150)])
    d.label(exp.cx + 8, 118, ["no answer", "by acceptBy"], 12, INK, anchor="start")
    d.line([ac.at("b", 0.5), (ac.cx, 150)])
    d.label(ac.cx + 8, 112, "cancel", 12, INK, mono=True, anchor="start")
    d.label(0, 232, ["acceptBy is 5 minutes after the order arrives by default.", "A customer can cancel only while the order is waiting; after accept only the merchant can."], 12, MUTED, anchor="start")
    return d


def vendor_day_calls():
    return steplist(
        "A vendor's day in calls",
        [
            ("Morning", [
                (1, "GET", "streetfood-vendor/session?outletId", "is a day already open?"),
                (2, "GET", "streetfood-vendor/spots?outletId", "pick a spot"),
                (3, "POST", "streetfood-vendor/session/start", "cart appears on customers' maps"),
            ]),
            ("All day", [
                (4, "POST", "streetfood-vendor/location/pings", "every nextIntervalSeconds, batched"),
                (5, "SOCKET", "streetfood-order:placed", "then GET streetfood-orders?view=waiting"),
                (6, "POST", "streetfood-orders/{id}/accept", "before acceptBy"),
                (7, "POST", "streetfood-orders/{id}/start", ""),
                (8, "POST", "streetfood-orders/{id}/ready", ""),
                (9, "POST", "streetfood-orders/{id}/complete", "paymentMethod for pay-at-pickup"),
                (10, "POST", "streetfood-vendor/session/relocate", "moved to another street (optional)"),
                (11, "PUT", "streetfood-vendor/session/sharing", "HIDDEN for a break (optional)"),
            ]),
            ("Evening", [
                (12, "POST", "streetfood-vendor/session/end", "the cart leaves the map"),
            ]),
        ],
        width=920,
    )


def story_overview():
    return sequence(
        "The whole story, from the vendor's morning to the customer's review",
        [A("v", "Vendor", "Ansilum app", "vendor"), A("p", "POS", "ansilum-be", "pos"), A("a", "Customer API", "", "api"), A("c", "Customer", "Goldenlane app", "customer")],
        [
            ("msg", "v", "p", "publish the outlet", False, 1),
            ("msg", "p", "a", "the outlet is listed (the operator registered the business)", False),
            ("msg", "v", "p", "open the day, share the GPS", False, 2),
            ("msg", "p", "a", "position events", False),
            ("msg", "a", "c", "the cart on the map, moving; the menu", True, 3),
            ("msg", "c", "a", "place an order", False, 4),
            ("msg", "a", "p", "the order (signed)", False),
            ("msg", "p", "v", "new order on the cashier screen", False),
            ("msg", "v", "p", "accept, prepare, mark ready", False, 5),
            ("msg", "p", "a", "status events", False),
            ("msg", "a", "c", "push + live status", True),
            ("note", ("p", "c"), "The customer walks to the cart, which is still moving, and picks up. Pays at pickup, or already paid online."),
            ("msg", "v", "p", "complete the order", False, 6),
            ("msg", "p", "a", "status event", False),
            ("msg", "a", "c", "the customer can rate the vendor", True),
            ("msg", "v", "p", "end the day", False, 7),
            ("msg", "p", "a", "position cleared", False),
            ("msg", "a", "c", "the cart leaves the map", True),
        ],
        gap=235,
    )


def vendor_start_day():
    return sequence(
        "Opening the day",
        [A("v", "Vendor", "Ansilum app", "vendor"), A("p", "POS", "", "pos"), A("a", "Customer API", "", "api")],
        [
            ("msg", "v", "p", "Start day: a saved spot or \"here\", and what to share", False, 1),
            ("note", "p", "opens the day (only one open day per outlet)"),
            ("msg", "p", "a", "event outlet.session.changed", False, 2),
            ("note", "a", "stores the working day and recomputes where customers can find this outlet"),
            ("note", "a", "The next customer search can show the cart", "ok"),
        ],
        gap=300,
    )


def vendor_gps_day():
    return sequence(
        "Broadcasting the position during the day",
        [A("ph", "Phone", "background", "vendor"), A("p", "POS", "", "pos"), A("a", "Customer API", "", "api"), A("c", "Customers", "", "customer")],
        [
            ("msg", "ph", "p", "readings (a batch)", False, 1),
            ("note", "p", "judge each reading (reject, flag, trust); keep the trail; current position = newest trusted reading; publish if 15 s and 20 m passed (parked: every 90 s at most)"),
            ("msg", "p", "ph", "send again in N seconds", True, 2),
            ("msg", "p", "a", "event outlet.location.updated", False, 3),
            ("note", "a", "stores it if it is newer"),
            ("msg", "c", "a", "next search", False, 4),
            ("msg", "a", "c", "the dot moves", True),
        ],
        gap=250,
    )


def vendor_orders_mini():
    d = D("An order, from the vendor's side")
    labels = [
        (["New order appears", "on the cashier screen"], "warn"),
        (["Accept", "(or reject)"], "pos"),
        (["Start preparing", "(stock is used)"], "pos"),
        (["Ready"], "pos"),
        (["Complete", "(the sale is recorded)"], "ok"),
    ]
    boxes = []
    for i, (lines, col) in enumerate(labels):
        boxes.append(d.box(i * 200, 0, 160, 60, lines, col, size=12.5))
    for i in range(4):
        d.line([boxes[i].r, boxes[i + 1].l])
    d.label(0, 96, "Accept before the order's acceptBy time (5 minutes by default), otherwise it expires and the customer is told to choose another place.", 12, MUTED, anchor="start")
    return d


def vendor_end_day():
    return sequence(
        "Closing the day",
        [A("v", "Vendor", "Ansilum app", "vendor"), A("p", "POS", "", "pos"), A("a", "Customer API", "", "api")],
        [
            ("msg", "v", "p", "End day", False, 1),
            ("note", "p", "closes the day and clears the position (done by the POS itself after 18 hours if forgotten)"),
            ("msg", "p", "a", "event outlet.location.cleared", False, 2),
            ("note", "a", "removes the position: the cart leaves the map", "ok"),
        ],
        gap=300,
    )


def customer_map_poll():
    return sequence(
        "Looking around the map",
        [A("app", "Goldenlane app", "map screen", "customer"), A("a", "Customer API", "", "api")],
        [
            ("msg", "app", "a", "GET /v1/outlets/nearby?lat&lng&radius_m=2000&open_now=true", False, 1),
            ("msg", "a", "app", "outlets, nearest first", True),
            ("section", "Every 15 seconds while the map is open"),
            ("msg", "app", "a", "the same request again", False, 2),
            ("msg", "a", "app", "new positions", True),
            ("note", "app", "the cart's marker glides to its new position", "ok"),
        ],
        gap=480,
    )


def customer_statuses():
    d = D("Order statuses as the customer sees them")
    bw, bh, gx = 164, 46, 36
    top = [("UNPAID", "warn"), ("CREATED", "plain"), ("PENDING_ACCEPTANCE", "warn"), ("ACCEPTED", "pos"), ("PREPARING", "pos"), ("READY", "pos"), ("COMPLETED", "ok")]
    boxes = [d.box(i * (bw + gx), 0, bw, bh, [n], c, size=11.5) for i, (n, c) in enumerate(top)]
    for i in range(len(top) - 1):
        d.line([boxes[i].r, boxes[i + 1].l], dashed=(i == 0))
    d.label((boxes[0].r[0] + boxes[1].l[0]) / 2, -30, ["paid", "(online orders only)"], 11, MUTED)
    sub = d.box(boxes[1].cx - 70, 150, 140, bh, ["SUBMIT_FAILED"], "bad", size=11.5)
    d.line([boxes[1].b, sub.t])
    d.label(boxes[1].cx, 214, ["the POS refused it,", "or could not be reached"], 11.5, MUTED)
    p = boxes[2]
    rej = d.box(p.x - 10, 150, 130, bh, ["REJECTED"], "bad", size=11.5)
    can = d.box(p.x + 150, 150, 130, bh, ["CANCELLED"], "bad", size=11.5)
    exp = d.box(p.x + 310, 150, 130, bh, ["EXPIRED"], "bad", size=11.5)
    for tgt in (rej, can, exp):
        d.line([p.b, (p.cx, 112), (tgt.cx, 112), (tgt.cx, 150)])
    d.label(rej.cx, 214, ["the merchant refused,", "with a reason"], 11.5, MUTED)
    d.label(can.cx, 214, ["cancelled by the customer", "or the merchant"], 11.5, MUTED)
    d.label(exp.cx, 214, ["no answer by acceptBy,", "or payment time ran out"], 11.5, MUTED)
    return d


def system_overview():
    d = D("The two systems in one picture")
    ph = d.box(0, 20, 190, 56, ["Ansilum vendor app"], "vendor", "Vendor phone")
    ca = d.box(0, 150, 190, 56, ["Ansilum cashier app"], "vendor", "Cashier tablet")
    pos = d.box(330, 0, 220, 230, ["NestJS", "PostgreSQL", "the source of truth"], "pos", "POS  (ansilum-be)")
    api = d.box(760, 0, 220, 230, ["Go", "PostgreSQL (read model)", "accounts, orders, payments"], "api", "This API")
    cust = d.box(1110, 85, 190, 60, ["sign-in with Firebase"], "customer", "Customer app")
    gw = d.box(760, 320, 220, 60, ["the merchant's own account"], "gateway", "Xendit / Midtrans")
    d.line([ph.r, (pos.x, 48)])
    d.label(260, 40, "GPS", 12, INK)
    d.line([ca.r, (pos.x, 178)])
    d.label(260, 170, "orders", 12, INK)
    x1, x2 = pos.x + pos.w, api.x
    d.line([(x1 + 6, 70), (x2 - 6, 70)])
    d.label((x1 + x2) / 2, 48, ["events", "positions, menus, orders…"], 12, INK)
    d.line([(x2 - 6, 130), (x1 + 6, 130)])
    d.label((x1 + x2) / 2, 108, ["orders, price quotes,", "state reads"], 12, INK)
    d.line([(x1 + 6, 190), (x2 - 6, 190)])
    d.label((x1 + x2) / 2, 168, ["fees", "reserve / report"], 12, INK)
    d.line([(cust.x - 6, 115), (api.x + api.w + 6, 115)], both=True)
    d.line([api.b, (api.cx, gw.y)], both=True)
    d.label(api.cx + 14, 285, ["charges, refunds, status", "callbacks"], 12, INK, anchor="start")
    return d


def neon_duty_cycle():
    d = D("How often the database is touched decides the bill")
    rows = [
        ("Any poller under 5 minutes", 100, "182 CU-h", "$19.35", "bad"),
        ("A job every 15 minutes", 33.3, "61 CU-h", "$6.45", "warn"),
        ("A job every 30 minutes", 16.7, "30 CU-h", "$3.22", "warn"),
        ("A job every hour", 8.3, "15 CU-h", "$1.61", "ok"),
        ("Once a night", 0.35, "0.6 CU-h", "$0.07", "ok"),
    ]
    left, width = 250, 420
    d.text(left, -8, "share of the month the compute is awake", 12, MUTED, anchor="start")
    d.text(left + width + 150, -8, "CU-hours and cost", 12, MUTED, anchor="end")
    y = 14
    for label, pct, cuh, cost, col in rows:
        d.text(0, y + 22, label, 13, INK, anchor="start")
        d.rect(left, y + 4, width, 26, "plain", rx=6, fill="#f1f5f9", stroke="#e2e8f0", sw=1)
        w = max(width * pct / 100, 3)
        d.rect(left, y + 4, w, 26, col, rx=6)
        d.text(left + min(w + 8, width - 44), y + 22, f"{pct:g}%", 12, INK, anchor="start", weight="bold")
        d.text(left + width + 150, y + 22, f"{cuh}, {cost}", 12.5, INK, anchor="end")
        y += 44
    d.label(0, y + 8, ["Every wake keeps the compute up for about 5 minutes (Neon's scale-to-zero timeout), so a job every 5 minutes or less is the same as always on.",
                       "Compute only, at the 0.25 CU minimum on the Launch plan ($0.106 per CU-hour). Storage and bursts above 0.25 CU come on top."], 11.5, MUTED, anchor="start")
    return d


def neon_target():
    d = D("Target: reads and live data stay off Neon, which wakes only for real writes")
    apps = d.box(0, 0, 200, 70, ["Customer app", "Ansilum vendor and cashier apps"], "customer", "Apps")
    svc = d.box(300, 0, 300, 150, ["answers reads from memory or Redis:", "outlets, menus, sessions, tenant checks,", "live vendor positions", "wakes its workers when work is created"], "api", "Customer API and POS backend")
    pg = d.box(760, 0, 240, 150, ["orders, payments, outbox, accounts", "", "sleeps between writes and a", "coarse sweep every 15 to 60 minutes"], "pos", "Neon Postgres")
    spool = d.box(300, 230, 300, 90, ["GPS trail rows wait here", "and are written in one batch", "every 15 minutes or at the end of the day"], "gateway", "SQLite spool or memory")
    redis = d.box(0, 230, 200, 90, ["only needed with more than one", "instance: signals, shared cache"], "firebase", "Redis (optional)")
    d.line([(apps.x + apps.w, 35), (svc.x, 35)])
    d.label(250, 22, ["reads,", "positions"], 12, INK)
    d.line([svc.r, (pg.x, 75)], dashed=True)
    d.label((svc.x + svc.w + pg.x) / 2, 62, ["writes,", "cache misses"], 12, INK)
    d.line([svc.b, spool.t], both=False)
    d.label(svc.cx + 10, 195, "position batches", 12, INK, anchor="start")
    d.line([spool.r, (pg.cx, 320), (pg.cx, pg.y + pg.h)], dashed=True)
    d.label(pg.cx - 8, 308, "one batch every 15 minutes", 12, INK, anchor="end")
    d.line([redis.r, spool.l], arrow=False, dashed=True)
    return d


DIAGRAMS = {
    "neon-duty-cycle": neon_duty_cycle,
    "neon-target": neon_target,
    "system-overview": system_overview,
    "architecture": architecture,
    "link-creation": link_creation,
    "push-pipeline": push_pipeline,
    "gps-reading": gps_reading,
    "position-rules": position_rules,
    "position-timeline": position_timeline,
    "order-pickup": order_pickup,
    "online-payment": online_payment,
    "fee-collection": fee_collection,
    "sign-in": sign_in,
    "customer-journey": customer_journey,
    "vendor-order-flow": vendor_order_flow,
    "vendor-day-calls": vendor_day_calls,
    "story-overview": story_overview,
    "vendor-start-day": vendor_start_day,
    "vendor-gps-day": vendor_gps_day,
    "vendor-orders-mini": vendor_orders_mini,
    "vendor-end-day": vendor_end_day,
    "customer-map-poll": customer_map_poll,
    "customer-statuses": customer_statuses,
}

if __name__ == "__main__":
    for name, fn in DIAGRAMS.items():
        build(name, fn())
