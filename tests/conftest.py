import os
import sys
import datetime
from zoneinfo import ZoneInfo

import pytest

# The bot reads these at import time; tests never talk to Discord or Google.
os.environ.setdefault("GUILD_ID", "1")
os.environ.setdefault("EVENTS_CHANNEL_ID", "2")
os.environ.setdefault("GOOGLE_CALENDAR_ID", "cal")
os.environ.setdefault("REMINDER_ROLE_WORKSHOPS", "111")
os.environ.setdefault("REMINDER_ROLE_SOCIALS", "222")
os.environ.setdefault("REMINDER_ROLE_PRODEV", "333")
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

TZ = ZoneInfo("America/New_York")


@pytest.fixture(autouse=True)
def data_dir(tmp_path, monkeypatch):
    """Each test gets an empty working directory, so data/*.json starts fresh."""
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    (tmp_path / "data").mkdir()
    (tmp_path / "bot").mkdir()
    (tmp_path / "bot" / "deadlines.json").write_text(open(os.path.join(root, "bot", "deadlines.json")).read())
    monkeypatch.chdir(tmp_path)
    return tmp_path


def make_event(name="Bash Scripting Workshop", location="ENG2 205", days=5, hours=2, description="Learn bash.", gid="g1"):
    start = datetime.datetime.now(TZ).replace(second=0, microsecond=0) + datetime.timedelta(days=days)
    return {"id": gid, "name": name, "location": location, "start": start,
            "end": start + datetime.timedelta(hours=hours), "description": description, "flyer": None}
