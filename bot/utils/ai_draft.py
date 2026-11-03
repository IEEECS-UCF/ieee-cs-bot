import os

import anthropic

# "Draft with AI" on an event card: Claude writes an #announcements post from
# the event's details, in the style of recent posts by officers. The draft
# still goes through officer review like any other (bot/officer/review.py).

MODEL = "claude-sonnet-5-5"
EXAMPLES = 12          # recent officer posts shown as the house style
EXAMPLE_MIN_CHARS = 80  # skip one-liners

SYSTEM = """You write #announcements posts for a university student chapter's Discord server.
Match the style of the example posts you're given: their layout, headings, emoticons, length and tone.

Rules:
- Use only the facts in the event details. Never invent speakers, food, prizes, prerequisites or anything else.
- If the event has no description, write one short, general sentence about the event from its name, and nothing more specific.
- Copy the date, time, room, link and opening mention exactly as given.
- Reply with the post text only, ready to paste into Discord. No preamble, no code fences."""

def configured():
    return bool(os.getenv("ANTHROPIC_API_KEY"))

def is_example(message):
    """A past announcement written by a person, long enough to show the style."""
    return not message.author.bot and len(message.content) >= EXAMPLE_MIN_CHARS

def prompt(event, description, discord_link, opener, examples):
    """The user turn: the example posts first (cached, since they rarely change between
    drafts), then this event's details."""
    shown = "\n\n".join(f"<example>\n{text}\n</example>" for text in examples) or "(none yet)"
    return [
        {"type": "text", "text": f"Recent posts in #announcements:\n{shown}", "cache_control": {"type": "ephemeral"}},
        {"type": "text", "text": details(event, description, discord_link, opener)},
    ]

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
    """(text, None) or (None, why it failed)."""
    client = anthropic.AsyncAnthropic()
    try:
        response = await client.beta.messages.create(
            model=MODEL,
            max_tokens=4000,
            output_config={"effort": "medium"},
            betas=["server-side-fallback-2026-07-01"],
            fallbacks="default",
            system=SYSTEM,
            messages=[{"role": "user", "content": prompt(event, description, discord_link, opener, examples)}],
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
