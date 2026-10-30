import datetime
import types

from bot.cogs import event_reminders as er


def ev(name, location):
    start = datetime.datetime(2026, 11, 13, 22, tzinfo=datetime.timezone.utc)
    return types.SimpleNamespace(id=1, name=name, location=location, start_time=start)

def test_tba_skipped_unless_forced():
    vulkan = ev("Intro to Vulkan", "TBA")
    assert er.should_send(vulkan, None)[0] is False
    assert er.should_send(vulkan, {"send": "always"})[0] is True

def test_never_send():
    assert er.should_send(ev("Fall GBM #3", "ENG2 205"), {"send": "never"})[0] is False

def test_auto_ping_by_kind():
    text, allowed = er.build_message(ev("Bash Scripting Workshop", "ENG2 205"), 1)
    assert text.startswith("<@&111>") and not allowed.everyone
    text, allowed = er.build_message(ev("Fall GBM #3", "ENG2 205"), 1)
    assert text.startswith("Hey everyone!")

def test_everyone_only_when_chosen():
    text, allowed = er.build_message(ev("Fall GBM #3", "ENG2 205"), 1, {"ping": "everyone"})
    assert text.startswith("@everyone") and allowed.everyone
    text, allowed = er.build_message(ev("Bash Scripting Workshop", "ENG2 205"), 1, {"ping": "none"})
    assert text.startswith("Hey everyone!") and not allowed.everyone
