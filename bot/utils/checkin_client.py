import io
import os
import json

import aiohttp
import segno

# Client for the check-in Apps Script web app (apps_script/checkin/Code.gs),
# which runs on the chapter Google account. It answers POSTs with JSON;
# Apps Script replies through a redirect, which aiohttp follows as a GET.
TIMEOUT = aiohttp.ClientTimeout(total=90)


class CheckinError(Exception):
    pass


def configured():
    return bool(os.getenv('CHECKIN_URL') and os.getenv('CHECKIN_SECRET'))

async def call(action, **params):
    payload = {"secret": os.getenv('CHECKIN_SECRET'), "action": action, **params}
    async with aiohttp.ClientSession() as session:
        async with session.post(os.getenv('CHECKIN_URL'), data=json.dumps(payload), timeout=TIMEOUT) as resp:
            text = await resp.text()
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        raise CheckinError(f"Unexpected reply from the check-in script (HTTP {resp.status}).")
    if 'error' in data:
        raise CheckinError(data['error'])
    return data

async def create_form(event, date, key):
    """Create (or fetch, if one exists for key) the check-in form for an event."""
    return await call("create", event=event, date=date, key=key)

async def results(form_id):
    return await call("results", formId=form_id)

def qr_png(url):
    """A QR code for the URL as PNG bytes, sized to read from across a room."""
    buffer = io.BytesIO()
    segno.make(url, error='m').save(buffer, kind='png', scale=12, border=2)
    return buffer.getvalue()

async def form_info(form_id):
    """Title and links of an existing form the chapter account can open."""
    return await call("info", formId=form_id)

def parse_form_id(text):
    """Accept a form ID or an edit link like docs.google.com/forms/d/<id>/edit."""
    text = (text or "").strip()
    if "/forms/d/e/" in text:
        return None  # a share link; it doesn't contain the ID the script needs
    if "/forms/d/" in text:
        text = text.split("/forms/d/", 1)[1].split("/", 1)[0]
    return text if len(text) > 20 and all(c.isalnum() or c in "-_" for c in text) else None
