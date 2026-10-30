import types

from bot.utils import alerts


class Channel:
    def __init__(self):
        self.sent = []
    async def send(self, text, **kwargs):
        self.sent.append(text)

def bot_with(channel):
    return types.SimpleNamespace(get_channel=lambda channel_id: channel)

async def test_same_problem_alerts_once_per_hour(monkeypatch):
    monkeypatch.setenv("ALERT_CHANNEL_ID", "5")
    monkeypatch.setattr(alerts, "_last_sent", {})
    channel = Channel()
    for _ in range(3):
        await alerts.alert(bot_with(channel), "Calendar sync", ValueError("boom"))
    await alerts.alert(bot_with(channel), "Officer digest", ValueError("boom"))
    assert len(channel.sent) == 2
    assert "Calendar sync" in channel.sent[0] and "boom" in channel.sent[0]

async def test_no_channel_means_log_only(monkeypatch):
    monkeypatch.delenv("ALERT_CHANNEL_ID", raising=False)
    monkeypatch.setattr(alerts, "_last_sent", {})
    await alerts.alert(bot_with(None), "Job feed", RuntimeError("x"))  # must not raise

def test_streak_alerts_on_third_failure_only():
    streak = alerts.Streak(limit=3)
    assert [streak.failed() for _ in range(4)] == [False, False, True, False]
    streak.ok()
    assert [streak.failed() for _ in range(3)] == [False, False, True]
