import os
import time
import traceback

import discord

# Short alerts in ALERT_CHANNEL_ID when something the bot does on its own fails,
# so a broken sync or digest doesn't go unnoticed in the server logs. The same
# problem alerts at most once per ALERT_EVERY seconds.
ALERT_EVERY = 3600
_last_sent = {}


def _channel(bot):
    channel_id = os.getenv('ALERT_CHANNEL_ID')
    return bot.get_channel(int(channel_id)) if channel_id else None

async def alert(bot, where, error=None, detail=None):
    """Print the failure, and post it to the alert channel unless it was posted recently."""
    summary = detail or (f"{type(error).__name__}: {error}" if error else "failed")
    print(f"{where} failed: {summary}")
    if error is not None:
        traceback.print_exception(error)
    key = f"{where}|{type(error).__name__ if error else summary}"
    now = time.monotonic()
    if now - _last_sent.get(key, -ALERT_EVERY) < ALERT_EVERY:
        return
    channel = _channel(bot)
    if channel is None:
        return
    _last_sent[key] = now
    try:
        await channel.send(f"⚠️ **{where}** failed: `{summary[:300]}`\nDetails are in the bot's log "
                           f"(`journalctl -u ieee-bot`). Same problem won't alert again for an hour.",
                           allowed_mentions=discord.AllowedMentions.none())
    except discord.HTTPException as e:
        print(f"Could not post alert: {e}")


class Streak:
    """Alert only after a failure repeats, for things that fail briefly now and then."""
    def __init__(self, limit=3):
        self.limit, self.count = limit, 0

    def failed(self):
        self.count += 1
        return self.count == self.limit

    def ok(self):
        self.count = 0
