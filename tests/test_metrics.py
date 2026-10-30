import csv
import datetime
import io
import types

from bot.officer import metrics, members
from bot.cogs import officer as officer_cog
from tests.officer_fixtures import NOW, event, snapshot, listing

TZ = NOW.tzinfo


def october():
    """Three October events: one reported in vTools, one counted by check-in, one not counted."""
    workshop = event(gid="w", name="Linux Workshop", days=-20)              # Oct 13
    gbm = event(gid="g", name="GBM #3", days=-10)                           # Oct 23
    social = event(gid="s", name="Game Night Social", days=-5)              # Oct 28
    november = event(gid="n", name="TechX", days=3)
    snap = snapshot([workshop, gbm, social, november],
                    vtools={"w": listing(workshop, "vtools", **{"ieee-attending": 12, "guests-attending": 8})},
                    checkins={"g": {"total": 30, "ieee": 10, "guests": 20, "heard": {"Discord": 18, "Instagram": 7}}},
                    store={"w": {"marks": {"recap": "done"}}})
    return snap

def test_month_helpers():
    start, end = metrics.month_span(2026, 12)
    assert start.isoformat().startswith("2026-12-01") and end.isoformat().startswith("2027-01-01")
    assert metrics.previous_month(2027, 1) == (2026, 12)

def test_summary_counts_events_attendance_and_follow_through():
    s = metrics.summarize(october(), 2026, 10, now=NOW)
    assert s["complete"] and s["events"] == 3 and s["counted"] == 2       # November's event is left out
    assert (s["attendance"], s["ieee"], s["guests"], s["average"]) == (50, 22, 28, 25.0)
    assert [r["source"] for r in s["rows"]] == ["vTools", "check-in", ""]
    assert s["kinds"]["Workshops"] == {"events": 1, "attendance": 20}
    assert s["heard"] == {"Discord": 18, "Instagram": 7}
    assert s["follow_up"]["attendance"] == [1, 3]                          # only the workshop is in vTools
    assert s["follow_up"]["slides"][1] == 2                                # the social doesn't need slides

def test_month_in_progress_is_not_remembered():
    s = metrics.summarize(october(), 2026, 11, now=NOW)
    assert not s["complete"]
    metrics.remember(s)
    assert metrics.remembered(2026, 11) is None
    metrics.remember(metrics.summarize(october(), 2026, 10, now=NOW))
    assert metrics.remembered(2026, 10)["attendance"] == 50 and "rows" not in metrics.remembered(2026, 10)

def test_membership_numbers_split_reviews_from_role_backfill():
    members.submit(1, "00000001", "", today=datetime.date(2026, 10, 5))
    members.decide(1, "verified", 9, today=datetime.date(2026, 10, 6))
    members.recognize(2, today=datetime.date(2026, 10, 30))
    members.submit(3, "00000003", "", today=datetime.date(2026, 10, 31))
    n = metrics.membership_numbers(2026, 10)
    assert n == {"verified_new": 1, "verified_from_role": 1, "verified_year": 2, "pending": 1}
    assert metrics.membership_numbers(2026, 11)["verified_new"] == 0

def test_discord_joins_count_people_still_here():
    at = lambda d: datetime.datetime(2026, 10, d, tzinfo=datetime.timezone.utc)
    guild = types.SimpleNamespace(members=[types.SimpleNamespace(bot=False, joined_at=at(3)),
                                           types.SimpleNamespace(bot=False, joined_at=at(1) - datetime.timedelta(days=40)),
                                           types.SimpleNamespace(bot=True, joined_at=at(4))])
    assert metrics.discord_numbers(guild, *metrics.month_span(2026, 10)) == {"discord_members": 2, "discord_joins": 1}

def test_report_compares_with_last_month():
    s = metrics.summarize(october(), 2026, 10, now=NOW)
    embeds = metrics.report_embeds(s, {"events": 2, "attendance": 60, "average": 30})
    text = embeds[0].description
    assert "October 2026" in embeds[0].title and "so far" not in embeds[0].title
    assert "**3** events (+1 vs last month)" in text and "(-10 vs last month)" in text
    assert "1 event(s) have no count yet" in text
    assert "Discord: 18" in embeds[2].description

def test_csv_has_one_row_per_event():
    data, name = metrics.csv_file(metrics.summarize(october(), 2026, 10, now=NOW))
    rows = list(csv.reader(io.StringIO(data.getvalue().decode())))
    assert name == "ieee-cs-events-2026-10.csv" and len(rows) == 4
    assert rows[1][:2] == ["2026-10-13", "Linux Workshop"] and rows[1][6] == "20" and rows[3][6] == ""

def test_report_posts_on_the_first():
    first = datetime.datetime(2026, 11, 1, 10, 5, tzinfo=TZ)
    assert officer_cog.report_due(first, None) == "2026-10"
    assert officer_cog.report_due(first, "2026-10") is None
    assert officer_cog.report_due(first.replace(hour=9), None) is None
    assert officer_cog.report_due(first.replace(day=2), None) is None
    assert officer_cog.report_due(datetime.datetime(2027, 1, 1, 11, tzinfo=TZ), None) == "2026-12"
