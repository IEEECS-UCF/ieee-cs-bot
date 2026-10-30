import datetime
import os
from google.oauth2 import service_account
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError

# Full scope so the bot can write back Discord changes; what it may actually do
# is still limited by how the calendar is shared with the service account.
SCOPES = ['https://www.googleapis.com/auth/calendar']
CREDENTIALS_FILE = 'credentials/google_service.json'

_service = None

def get_calendar_service():
    """Authenticates and returns the (cached) Google Calendar service object."""
    global _service
    if _service is None:
        if not os.path.exists(CREDENTIALS_FILE):
            raise FileNotFoundError(f"Missing Google credentials at {CREDENTIALS_FILE}")

        creds = service_account.Credentials.from_service_account_file(
            CREDENTIALS_FILE, scopes=SCOPES
        )
        _service = build('calendar', 'v3', credentials=creds, cache_discovery=False)
    return _service

def fetch_upcoming_events(calendar_id: str, days: int = 7):
    """Fetches all events (every page) from now through the next 'days'.

    Returns (items, calendar_time_zone, access_role); access_role is 'reader',
    'writer' or 'owner' depending on how the calendar is shared with us.
    """
    service = get_calendar_service()

    now = datetime.datetime.now(datetime.timezone.utc)
    time_min = now.isoformat()
    time_max = (now + datetime.timedelta(days=days)).isoformat()

    items = []
    time_zone = 'UTC'
    access_role = 'reader'
    page_token = None
    while True:
        result = service.events().list(
            calendarId=calendar_id,
            timeMin=time_min,
            timeMax=time_max,
            singleEvents=True,
            orderBy='startTime',
            maxResults=250,
            pageToken=page_token,
        ).execute(num_retries=3)
        time_zone = result.get('timeZone') or time_zone
        access_role = result.get('accessRole') or access_role
        items.extend(result.get('items', []))
        page_token = result.get('nextPageToken')
        if not page_token:
            break

    return items, time_zone, access_role

def get_event(calendar_id: str, event_id: str):
    """Fetches a single event by ID. Returns None if it was deleted."""
    service = get_calendar_service()
    try:
        return service.events().get(calendarId=calendar_id, eventId=event_id).execute(num_retries=3)
    except HttpError as e:
        if e.resp.status in (404, 410):
            return None
        raise

def insert_event(calendar_id: str, body: dict):
    service = get_calendar_service()
    return service.events().insert(calendarId=calendar_id, body=body).execute(num_retries=3)

def patch_event(calendar_id: str, event_id: str, body: dict):
    service = get_calendar_service()
    return service.events().patch(calendarId=calendar_id, eventId=event_id, body=body).execute(num_retries=3)

def delete_event(calendar_id: str, event_id: str):
    """Deleted events stay in Google Calendar's trash for 30 days."""
    service = get_calendar_service()
    try:
        service.events().delete(calendarId=calendar_id, eventId=event_id).execute(num_retries=3)
    except HttpError as e:
        if e.resp.status not in (404, 410):
            raise
