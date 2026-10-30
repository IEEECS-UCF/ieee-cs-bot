import os
import datetime

from dateutil import parser

from bot.utils.formatting import create_base_embed, IEEE_BLUE, IEEE_GOLD
from bot.officer.data import load_deadlines
from bot.officer.sources import TZ
from bot.officer.steps import PHASES, LEGEND, PROBLEMS, event_steps, is_open, problems, owner_of

# Turning steps into text. Every view uses the same icons and words.

EMBED_LIMIT = 4000
MESSAGE_EMBED_CHARS = 5800  # Discord allows 6000 across all embeds in one message
MESSAGE_EMBEDS = 10
DEADLINE_DAYS = 90
TABLING_DAYS = 60


def fmt_day(dt):
    return dt.strftime('%a %b %-d')

def build_embed(title, lines, color=IEEE_BLUE, footer=None):
    text = ""
    for line in lines:
        if len(text) + len(line) + 1 > EMBED_LIMIT:
            text += "\n…"
            break
        text += line + "\n"
    embed = create_base_embed(title=title, description=text or "Nothing here.", color=color)
    if footer:
        embed.set_footer(text=footer)
    return embed

def embed_size(embed):
    return len(embed.title or "") + len(embed.description or "") + len(embed.footer.text or "") + \
        sum(len(f.name) + len(f.value) for f in embed.fields)

def split_embeds(embeds):
    """Group embeds into messages under Discord's per-message limits."""
    batches, current, size = [], [], 0
    for embed in embeds:
        n = embed_size(embed)
        if current and (size + n > MESSAGE_EMBED_CHARS or len(current) == MESSAGE_EMBEDS):
            batches.append(current)
            current, size = [], 0
        current.append(embed)
        size += n
    if current:
        batches.append(current)
    return batches


# ---------- board ----------

def phase_icons(steps):
    return " · ".join(f"{phase} {''.join(s.state for s in steps if s.phase == phase)}" for phase in PHASES)

def board_rows(snap, now=None):
    """[(event, steps)] for events on the board, soonest first."""
    now = now or datetime.datetime.now(TZ)
    rows = [(e, event_steps(e, snap, now)) for e in snap.events]
    return sorted([(e, s) for e, s in rows if is_open(e, s, now)], key=lambda r: r[0]['start'])

def board_line(e, steps, snap):
    owner, _ = owner_of(e, snap.store.get(e['id'], {}))
    todo = len(problems(steps))
    return (f"`{fmt_day(e['start'])}` **{e['name']}** · " + (f"<@{owner}>" if owner else "no owner")
            + (f" · **{todo} to do**" if todo else " · ✅ all set") + f"\n{phase_icons(steps)}")

def orphan_lines(snap):
    return [f"⚠️ **{l['name']}** ({fmt_day(parser.isoparse(l['startsOn']).astimezone(TZ))}) is on KnightConnect "
            f"({l.get('location') or 'no room'}) but not in the calendar · <{l['link']}>"
            for l in snap.knightconnect_orphans]

def board_embed(snap, rows, problems_only=False):
    lines = [board_line(e, s, snap) for e, s in rows if not problems_only or problems(s)]
    lines += orphan_lines(snap)
    if not lines:
        lines = ["Every event is on track. ✅"]
    footer = LEGEND + (f"\nCouldn't read: {'; '.join(snap.problems)}" if snap.problems else "")
    return build_embed("📋 Events", lines, footer=footer)


# ---------- event card ----------

def card_embed(e, steps, snap):
    record = snap.store.get(e['id'], {})
    owner, by_hand = owner_of(e, record)
    head = [f"**{fmt_day(e['start'])}**, {e['start']:%-I:%M}–{e['end']:%-I:%M %p} · {e['location'] or 'no room'}",
            "Owner: " + (f"<@{owner}>" + ("" if by_hand else " (default)") if owner else "nobody yet")]
    did = snap.discord_ids.get(e['id'])
    if did:
        head.append(f"https://discord.com/events/{os.getenv('GUILD_ID')}/{did}")
    embed = create_base_embed(title=e['name'], description="\n".join(head), color=IEEE_BLUE)
    for phase in PHASES:
        lines = [s.line() for s in steps if s.phase == phase]
        embed.add_field(name=phase, value="\n".join(lines)[:1024] or "—", inline=False)
    stamp = snap.fetched_at.strftime('%-I:%M %p') if snap.fetched_at else "?"
    embed.set_footer(text=f"{LEGEND}\nUse the menu below to fix anything · data from {stamp}"
                     + (f" · couldn't read {', '.join(p.split(':')[0] for p in snap.problems)}" if snap.problems else ""))
    return embed


# ---------- chapter ----------

def deadline_lines(today):
    lines = []
    for day, what in load_deadlines():
        left = (day - today).days
        if 0 <= left <= DEADLINE_DAYS:
            lines.append(f"`{day:%a %b %-d}` ({left}d) {what}")
    return lines or [f"No deadlines in the next {DEADLINE_DAYS} days."]

def tabling_lines(rows, today):
    lines = []
    for r in rows:
        try:
            day = datetime.date.fromisoformat(r.get('Date', '').strip())
        except ValueError:
            continue
        if not 0 <= (day - today).days <= TABLING_DAYS:
            continue
        filled = [r.get(k, '').strip() for k in ('Volunteer 1', 'Volunteer 2') if r.get(k, '').strip()]
        mark = "🔴" if not filled else ("🟡" if len(filled) < 2 else "🟢")
        lines.append(f"{mark} `{day:%a %b %-d}` {r.get('Event Name', '').strip()} {r.get('Time', '').strip()}: "
                     + (", ".join(filled) or "**nobody yet**"))
    return lines or ["No tabling sessions in the next two months."]

def task_lines(tasks):
    lines = [f"**{t['title']}** ({len(t['claimed'])}/{t['slots']}): " + (", ".join(f"<@{u}>" for u in t["claimed"]) or "nobody yet")
             for t in tasks.values() if t["open"]]
    return lines or ["No open volunteer tasks."]

def member_lines(verified, pending, guild_id, year):
    lines = [f"**{verified}** verified for {year} · **{len(pending)}** waiting for review"]
    for uid, record in sorted(pending, key=lambda p: p[1]["submitted"]):
        card = record.get("card")
        link = f" · [review](https://discord.com/channels/{guild_id}/{card['channel']}/{card['message']})" if card else ""
        lines.append(f"⬜ <@{uid}> `{record['number']}` since {record['submitted']}{link}")
    return lines

def chapter_embeds(tabling_rows, tasks, today, membership=None, guild_id=None):
    embeds = [build_embed("⏰ Deadlines", deadline_lines(today), IEEE_GOLD),
              build_embed("🪧 Tabling", tabling_lines(tabling_rows, today) if tabling_rows is not None
                          else ["Tabling sheet not configured or unreadable."]),
              build_embed("🙋 Volunteer tasks", task_lines(tasks))]
    if membership is not None:
        embeds.append(build_embed("🪪 IEEE members", member_lines(*membership, guild_id, today.year)))
    return embeds


# ---------- nudges ----------

def nudge_text(e, items, owner):
    who = f"<@{owner}> " if owner else "No owner yet. "
    return (who + f"Still open for **{e['name']}** ({fmt_day(e['start'])}):\n"
            + "\n".join(f"{s.state} **{s.label}**" + (f": {s.detail}" if s.detail else "") for s in items))
