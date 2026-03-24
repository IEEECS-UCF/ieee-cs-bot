import datetime
import os
from google.oauth2 import service_account
from googleapiclient.discovery import build

SCOPES = ['https://www.googleapis.com/auth/calendar.readonly']
CREDENTIALS_FILE = 'credentials/google_service.json'

def get_calendar_service():
    """Authenticates and returns the Google Calendar service object."""
    if not os.path.exists(CREDENTIALS_FILE):
        raise FileNotFoundError(f"Missing Google credentials at {CREDENTIALS_FILE}")

    creds = service_account.Credentials.from_service_account_file(
        CREDENTIALS_FILE, scopes=SCOPES
    )
    return build('calendar', 'v3', credentials=creds)

def fetch_upcoming_events(calendar_id: str, days: int = 7):
    """Fetches events from the specified calendar for the next 'days'."""
    service = get_calendar_service()
    
    now = datetime.datetime.utcnow().isoformat() + 'Z' 
    end_time = (datetime.datetime.utcnow() + datetime.timedelta(days=days)).isoformat() + 'Z'

    events_result = service.events().list(
        calendarId=calendar_id,
        timeMin=now,
        timeMax=end_time,
        singleEvents=True,
        orderBy='startTime'
    ).execute()

    return events_result.get('items', [])
