import re
from bs4 import BeautifulSoup

# Text built from a calendar event (the dicts officer_tools.fetch_calendar
# returns): copy-paste blocks for KnightConnect and vTools, and drafts for
# #announcements and Instagram. Nothing here posts anything.

KNIGHTCONNECT_EVENTS = "https://knightconnect.campuslabs.com/engage/actioncenter/organization/ieee-cs/events"
VTOOLS_CREATE = "https://events.vtools.ieee.org/tego_/event/create"
FLYER_LINE = re.compile(r'^\s*flyer:\s*\S+\s*$', re.IGNORECASE | re.MULTILINE)

# Spellings of the same building, so "ENGII 102" and "Engineering 2 Room 102" match
BUILDINGS = [
    (r'L3HARRIS|\bHEC\b', 'HEC'),
    (r'\bENGINEERING\s*(II|2)\b|\bENG\s*(II|2)\b|\bEGN\s*2\b', 'ENG2'),
    (r'\bENGINEERING\s*(I|1)\b|\bENG\s*(I|1)\b|\bEGN\s*1\b', 'ENG1'),
    (r'\bHEALTH\s*SCIENCES?\s*(I|1)\b|\bHS\s*1\b|\bHSB\b', 'HS1'),
    (r'\bHEALTH\s*SCIENCES?\s*(II|2)\b|\bHS\s*2\b', 'HS2'),
    (r'\bBUSINESS\s*ADMIN\w*\s*(I|1)\b|\bBA\s*1\b', 'BA1'),
    (r'\bCLASSROOM\s*BUILDING\s*(I|1)\b|\bCB\s*1\b', 'CB1'),
    (r'\bCLASSROOM\s*BUILDING\s*(II|2)\b|\bCB\s*2\b', 'CB2'),
    (r'\bTEACHING\s*ACADEMY\b|\bTA\b', 'TA'),
    (r'\bTECHNOLOGY\s*COMMONS\b|\bTCH\b', 'TCH'),
    (r'\bSTUDENT\s*UNION\b', 'SU'),
]

def place(location):
    """(building code or None, room number or None) from a free-text location."""
    text = (location or '').upper()
    building = next((code for pattern, code in BUILDINGS if re.search(pattern, text)), None)
    room = re.search(r'(?<![A-Z0-9])(\d{2,4})[A-Z]{0,2}(?![0-9])', re.sub(r'ENG\s*[12]|EGN\s*[12]|HS\s*[12]|BA\s*[12]|CB\s*[12]', ' ', text))
    return building, room.group(1) if room else None

def same_place(a, b):
    (building_a, room_a), (building_b, room_b) = place(a), place(b)
    if room_a and room_b and room_a != room_b:
        return False
    if building_a and building_b and building_a != building_b:
        return False
    return True

BUILDING_NAMES = {
    'ENG1': 'Engineering I', 'ENG2': 'Engineering II', 'HEC': 'L3Harris Engineering Center (HEC)',
    'HS1': 'Health Sciences I', 'HS2': 'Health Sciences II', 'BA1': 'Business Administration I',
    'CB1': 'Classroom Building I', 'CB2': 'Classroom Building II', 'TA': 'Teaching Academy',
    'TCH': 'Technology Commons', 'SU': 'Student Union',
}
UCF_ADDRESS = "12760 Pegasus Dr, Orlando, FL 32816"  # as on our existing vTools events
CONTACT_EMAIL = "ieeecsucf@gmail.com"

ROLE_WORDS = [
    ('workshops', ('workshop', 'intro to', 'scripting', 'tutorial')),
    ('socials', ('social', 'game night', 'picnic', 'party', 'mixer', 'bbq')),
    ('prodev', ('linkedin', 'career', 'resume', 'interview', 'panel')),
]


def plain_description(event):
    """The calendar description as plain text, without the Flyer line."""
    text = BeautifulSoup(event.get('description') or '', "html.parser").get_text(separator='\n')
    text = FLYER_LINE.sub('', text)
    return "\n".join(line.strip() for line in text.splitlines() if line.strip())

def when(event):
    start, end = event['start'], event['end']
    return f"{start:%A, %B %-d} · {start:%-I:%M %p}–{end:%-I:%M %p}"

def kind(event):
    """'workshops', 'socials', 'prodev' or None, from words in the name."""
    name = event['name'].casefold()
    return next((key for key, words in ROLE_WORDS if any(w in name for w in words)), None)

def copy_block(event, platform):
    """Fields laid out in the order the KnightConnect / vTools forms ask for them."""
    description = plain_description(event) or "(no description in the calendar yet)"
    start, end = event['start'], event['end']
    lines = [
        f"**Title:** {event['name']}",
        f"**Starts:** {start:%m/%d/%Y %-I:%M %p}",
        f"**Ends:** {end:%m/%d/%Y %-I:%M %p}",
        f"**Location:** {event['location'] or 'TBA'}",
        f"**Description:**\n```\n{description[:1500]}\n```",
    ]
    if platform == "knightconnect":
        lines.append(f"**Flyer:** {event.get('flyer') or 'none in the calendar (add a Flyer: line there)'}")
        lines.append(f"Create it here: {KNIGHTCONNECT_EVENTS} (Create Event)")
        return "\n".join(lines)
    # Laid out like vTools' create form; its required fields are marked *
    building, room = place(event['location'])
    category = {"socials": "Nontechnical", "prodev": "Professional"}.get(kind(event), "Technical")
    lines = [
        "**Host Organizational Unit*:** University of Central Florida, C16 (SBC13081)",
        f"**Host Contact Email*:** {CONTACT_EMAIL}",
        f"**Title*:** {event['name']}",
        f"**Category*:** {category} (suggested)",
        f"**Start time:** {start:%m/%d/%Y %-I:%M %p}",
        f"**End time*:** {end:%m/%d/%Y %-I:%M %p}",
        "**Time Zone:** America/New_York",
        f"**Description*:**\n```\n{description[:1500]}\n```",
        "**Tags/Keywords*:** #computer-society",
        "**Location:** In-Person",
        f"**Address Line 1:** {UCF_ADDRESS.split(',')[0]}",
        "**City*:** Orlando · **Country/State*:** United States, Florida · **Postal Code:** 32816",
        f"**Building:** {BUILDING_NAMES.get(building, event['location'] or 'TBA')}",
        f"**Room Number:** {room or '(check the calendar)'}",
    ]
    lines += [
        f"**Registration:** open now, close when the event starts ({start:%m/%d/%Y %-I:%M %p})",
        f"Create it here: {VTOOLS_CREATE}",
        "After the event, file the report with attendance (`/officer checkin results` has the counts).",
    ]
    return "\n".join(lines)

def announcement_draft(event, discord_link, role_mention=None):
    """An #announcements post in the chapter's house style."""
    opener = role_mention or "@everyone"
    description = plain_description(event)
    body = description if description else "[One or two sentences on what people will learn or do.]"
    return (
        f"# Hey {opener}!\n# ( ´ ▽ ` )ﾉ\n\n"
        f"## {event['name']}\n"
        f"{body}\n\n"
        f"📅 {event['start']:%A, %-m/%-d}\n"
        f"🕡 {event['start']:%-I:%M %p}\n"
        f"📍 {event['location'] or 'TBA'}\n"
        f"{discord_link}\n\n"
        f"Bring a friend! ( ๑>ᴗ<๑ )\n\n"
        f"## See You There!\nヾ(＾-＾)ノ"
    )

def instagram_caption(event):
    description = plain_description(event).split("\n")[0][:180]
    tags = {"workshops": "#ucf #ieee #workshop", "socials": "#ucf #ieee #social",
            "prodev": "#ucf #ieee #career"}.get(kind(event), "#ucf #ieee #ieeecs")
    return (f"{event['name']} · {event['start']:%a %-m/%-d}, {event['start']:%-I:%M %p}, "
            f"{event['location'] or 'TBA'}. {description} Link in bio! {tags}").replace("  ", " ")
