import datetime
import os
from google.oauth2 import service_account
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError

SCOPES = ['https://www.googleapis.com/auth/calendar.readonly']
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

    Returns (items, calendar_time_zone).
    """
    service = get_calendar_service()

    now = datetime.datetime.now(datetime.timezone.utc)
    time_min = now.isoformat()
    time_max = (now + datetime.timedelta(days=days)).isoformat()

    items = []
    time_zone = 'UTC'
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
        items.extend(result.get('items', []))
        page_token = result.get('nextPageToken')
        if not page_token:
            break

    return items, time_zone

def get_event(calendar_id: str, event_id: str):
    """Fetches a single event by ID. Returns None if it was deleted."""
    service = get_calendar_service()
    try:
        return service.events().get(calendarId=calendar_id, eventId=event_id).execute(num_retries=3)
    except HttpError as e:
        if e.resp.status in (404, 410):
            return None
        raise
