import os
import json

import anthropic

# "Draft with AI": Claude writes an #announcements post, from an event's details
# (event card) or an officer's notes (/officer announce), in the style of a fixed
# set of the chapter's best past posts (chosen by the chair; the Go bot embeds the
# same file). The draft still goes through officer review like any other
# (bot/officer/review.py).

MODEL = "claude-sonnet-5-5"
EXAMPLES_FILE = os.path.join(os.path.dirname(__file__), "..", "..", "internal", "aidraft", "announcement_examples.json")

SYSTEM = """You write #announcements posts for a university student chapter's Discord server.
Match the style of the example posts you're given (the chapter's best past announcements): their layout, headings, emoticons, length and tone.
Format with Discord markdown: a # or ## heading for the event or topic, **bold** for the date, time and room, and - bullet lists when there are several items.

Rules:
- Use only the facts in the event details. Never invent speakers, food, prizes, prerequisites or anything else.
- If the event has no description, write one short, general sentence about the event from its name, and nothing more specific.
- Copy the date, time, room, link and opening mention exactly as given.
- When writing from an officer's notes instead of an event, use only what the notes say, and don't add @everyone or any role mention unless the notes include it.
- Reply with the post text only, ready to paste into Discord. No preamble, no code fences."""

def configured():
    return bool(os.getenv("ANTHROPIC_API_KEY"))

def example_posts():
    """The chosen example announcements, in the file's order."""
    with open(EXAMPLES_FILE, encoding="utf-8") as f:
        return [e["text"] for e in json.load(f)]

def prompt(examples, ask):
    """The user turn: the example posts first (cached, since they never change between
    drafts), then what to write."""
    shown = "\n\n".join(f"<example>\n{text}\n</example>" for text in examples) or "(none yet)"
    return [
        {"type": "text", "text": f"Example posts from #announcements:\n{shown}", "cache_control": {"type": "ephemeral"}},
        {"type": "text", "text": ask},
    ]

def from_notes(notes):
    return f"Write an announcement from these notes by an officer:\n<notes>\n{notes}\n</notes>"

def details(event, description, discord_link, opener):
    return (f"Write the announcement for this event:\n"
            f"Name: {event['name']}\n"
            f"Date: {event['start']:%A, %-m/%-d}\n"
            f"Time: {event['start']:%-I:%M %p} to {event['end']:%-I:%M %p}\n"
            f"Room: {event['location'] or 'TBA'}\n"
            f"Description: {description or '(none)'}\n"
            f"Discord event link: {discord_link or '(none)'}\n"
            f"Open with this mention: {opener}")

async def draft(event, description, discord_link, opener, examples):
    """An event's announcement: (text, None) or (None, why it failed)."""
    return await write(prompt(examples, details(event, description, discord_link, opener)))

async def draft_from_notes(notes, examples):
    """A general announcement from an officer's notes: (text, None) or (None, why it failed)."""
    return await write(prompt(examples, from_notes(notes)))

async def write(content):
    client = anthropic.AsyncAnthropic()
    try:
        response = await client.beta.messages.create(
            model=MODEL,
            max_tokens=4000,
            output_config={"effort": "medium"},
            betas=["server-side-fallback-2026-07-01"],
            fallbacks="default",
            system=SYSTEM,
            messages=[{"role": "user", "content": content}],
        )
        u = response.usage
        print(f"AI draft usage: {u.input_tokens} new + {u.cache_read_input_tokens or 0} cached + "
              f"{u.cache_creation_input_tokens or 0} cache-write input tokens, {u.output_tokens} output")
    except anthropic.RateLimitError:
        return None, "Claude is busy right now. Try again in a minute."
    except anthropic.APIStatusError as e:
        return None, f"Claude returned an error ({e.status_code})."
    except anthropic.APIConnectionError:
        return None, "Couldn't reach Claude."
    if response.stop_reason == "refusal":
        return None, "Claude declined to write this one. Use the regular draft instead."
    text = "".join(b.text for b in response.content if b.type == "text").strip()
    if not text:
        return None, "Claude didn't return a draft."
    if response.stop_reason == "max_tokens":
        return None, "Claude's draft came out too long. Try again."
    return text[:2000], None
