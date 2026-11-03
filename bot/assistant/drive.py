import io
import os

from google.oauth2.credentials import Credentials
from google.auth.transport.requests import Request
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError
from googleapiclient.http import MediaIoBaseUpload

# Google Drive, Docs and Sheets for /officer ask, as ieeecsucf@gmail.com. It
# can't be the bot's service account: Google gives service accounts no storage
# in a personal Drive, so they can't upload there. The chapter account signs in
# once with scripts/drive_login.py, which saves its refresh token in
# DRIVE_TOKEN_FILE. Everything here blocks; call it with asyncio.to_thread.
# "Delete" only ever moves to Drive's trash, where it can be restored for 30 days.

DRIVE_TOKEN_FILE = "credentials/drive_user.json"
SCOPES = ["https://www.googleapis.com/auth/drive"]  # also covers the Docs and Sheets APIs
FOLDER = "application/vnd.google-apps.folder"
DOC = "application/vnd.google-apps.document"
SHEET = "application/vnd.google-apps.spreadsheet"
MAX_CHARS = 200_000   # per document, for the knowledge cache
READ_CHARS = 30_000   # what one read hands to Claude
EXPORT = {
    DOC: "text/plain",
    "application/vnd.google-apps.presentation": "text/plain",
    SHEET: "text/csv",
}
PLAIN = ("text/plain", "text/markdown", "text/csv", "application/json")
FIELDS = "id, name, mimeType, modifiedTime, webViewLink, parents, size"
KINDS = {"folder": FOLDER, "doc": DOC, "sheet": SHEET, "slides": "application/vnd.google-apps.presentation",
         "pdf": "application/pdf", "image": "image/"}


def configured():
    return os.path.exists(DRIVE_TOKEN_FILE)

def credentials():
    creds = Credentials.from_authorized_user_file(DRIVE_TOKEN_FILE, SCOPES)
    if not creds.valid:
        creds.refresh(Request())
    return creds

def service(api="drive", version="v3"):
    return build(api, version, credentials=credentials(), cache_discovery=False)

def escape(text):
    return text.replace("\\", "\\\\").replace("'", "\\'")

def listing(svc, query, limit=None):
    files, token = [], None
    while True:
        page = svc.files().list(q=query, pageSize=200, pageToken=token, fields=f"nextPageToken, files({FIELDS})",
                                orderBy="modifiedTime desc").execute(num_retries=3)
        files += page.get("files", [])
        token = page.get("nextPageToken")
        if not token or (limit and len(files) >= limit):
            return files[:limit] if limit else files

def describe(f):
    kind = next((k for k, m in KINDS.items() if f["mimeType"].startswith(m)), f["mimeType"].split("/")[-1])
    return f"{f['id']} | {f['name']} | {kind} | changed {f['modifiedTime'][:10]} | {f.get('webViewLink', '')}"


# ---------- reading ----------

def search(text="", kind="any", folder_id=None, limit=25):
    """Files and folders by name or contents, most recently changed first."""
    query = ["trashed = false"]
    if text.strip():
        t = escape(text.strip())
        query.append(f"(name contains '{t}' or fullText contains '{t}')")
    if kind in KINDS:
        query.append(f"mimeType contains '{KINDS[kind]}'" if kind == "image" else f"mimeType = '{KINDS[kind]}'")
    if folder_id:
        query.append(f"'{escape(folder_id)}' in parents")
    return [describe(f) for f in listing(service(), " and ".join(query), limit)]

def info(file_id):
    return service().files().get(fileId=file_id, fields=FIELDS).execute(num_retries=3)

def read(file_id):
    """(name, link, text) of a Doc, Sheet, Slides or plain text file."""
    svc = service()
    f = svc.files().get(fileId=file_id, fields=FIELDS).execute(num_retries=3)
    if f["mimeType"] in EXPORT:
        data = svc.files().export(fileId=file_id, mimeType=EXPORT[f["mimeType"]]).execute(num_retries=3)
    elif f["mimeType"] in PLAIN:
        data = svc.files().get_media(fileId=file_id).execute(num_retries=3)
    else:
        raise ValueError(f"{f['name']} is a {f['mimeType']} file, which can't be read as text")
    text = data.decode("utf-8", errors="replace") if isinstance(data, bytes) else str(data)
    return f["name"], f.get("webViewLink", ""), text[:READ_CHARS]

def sheet_values(sheet_id, cell_range=""):
    """Rows of a sheet (the first tab unless a range like 'Attendance!A1:F50' is given)."""
    sheets = service("sheets", "v4").spreadsheets()
    if not cell_range:
        meta = sheets.get(spreadsheetId=sheet_id, fields="sheets.properties.title").execute(num_retries=3)
        cell_range = meta["sheets"][0]["properties"]["title"]
    return sheets.values().get(spreadsheetId=sheet_id, range=cell_range).execute(num_retries=3).get("values", [])

def changed_documents(seen):
    """Changed readable files for the knowledge cache: [(id, title, url, modified, text)], and every id there.
    seen: {file id: modifiedTime} from the last refresh."""
    svc = service()
    files = listing(svc, f"trashed = false and mimeType != '{FOLDER}'")
    changed = []
    for f in files:
        if seen.get(f["id"]) == f["modifiedTime"] or (f["mimeType"] not in EXPORT and f["mimeType"] not in PLAIN):
            continue  # unchanged, or a PDF/image/etc. that isn't worth an entry by name alone
        try:
            if f["mimeType"] in EXPORT:
                data = svc.files().export(fileId=f["id"], mimeType=EXPORT[f["mimeType"]]).execute(num_retries=3)
            else:
                data = svc.files().get_media(fileId=f["id"]).execute(num_retries=3)
        except HttpError as e:
            print(f"Knowledge: couldn't read Drive file {f['name']}: {e.status_code}")
            continue
        text = data.decode("utf-8", errors="replace") if isinstance(data, bytes) else str(data)
        changed.append((f["id"], f["name"], f.get("webViewLink", ""), f["modifiedTime"], text[:MAX_CHARS]))
    return changed, {f["id"] for f in files}


# ---------- writing ----------

def upload(data, filename, mime_type, folder_id):
    media = MediaIoBaseUpload(io.BytesIO(data), mimetype=mime_type or "application/octet-stream", resumable=len(data) > 5_000_000)
    return service().files().create(body={"name": filename, "parents": [folder_id]}, media_body=media,
                                    fields="webViewLink").execute(num_retries=3)["webViewLink"]

def create_folder(name, parent_id=None):
    body = {"name": name, "mimeType": FOLDER, **({"parents": [parent_id]} if parent_id else {})}
    return service().files().create(body=body, fields="webViewLink").execute(num_retries=3)["webViewLink"]

def create_doc(title, text, folder_id=None):
    """A new Google Doc with the text (Drive converts it); returns its link."""
    media = MediaIoBaseUpload(io.BytesIO(text.encode()), mimetype="text/plain", resumable=False)
    body = {"name": title, "mimeType": DOC, **({"parents": [folder_id]} if folder_id else {})}
    return service().files().create(body=body, media_body=media, fields="webViewLink").execute(num_retries=3)["webViewLink"]

def append_to_doc(doc_id, text):
    docs = service("docs", "v1").documents()
    docs.batchUpdate(documentId=doc_id, body={"requests": [
        {"insertText": {"endOfSegmentLocation": {}, "text": "\n" + text}}]}).execute(num_retries=3)

def append_rows(sheet_id, rows, tab=""):
    """Add rows under the last filled row of a tab (the first tab by default)."""
    sheets = service("sheets", "v4").spreadsheets()
    if not tab:
        meta = sheets.get(spreadsheetId=sheet_id, fields="sheets.properties.title").execute(num_retries=3)
        tab = meta["sheets"][0]["properties"]["title"]
    sheets.values().append(spreadsheetId=sheet_id, range=tab, valueInputOption="USER_ENTERED",
                           insertDataOption="INSERT_ROWS", body={"values": rows}).execute(num_retries=3)

def move(file_id, folder_id):
    svc = service()
    old = ",".join(svc.files().get(fileId=file_id, fields="parents").execute(num_retries=3).get("parents", []))
    svc.files().update(fileId=file_id, addParents=folder_id, removeParents=old, fields="id").execute(num_retries=3)

def rename(file_id, name):
    service().files().update(fileId=file_id, body={"name": name}, fields="id").execute(num_retries=3)

def copy(file_id, name=None, folder_id=None):
    body = {**({"name": name} if name else {}), **({"parents": [folder_id]} if folder_id else {})}
    return service().files().copy(fileId=file_id, body=body, fields="webViewLink").execute(num_retries=3)["webViewLink"]

def trash(file_id):
    service().files().update(fileId=file_id, body={"trashed": True}, fields="id").execute(num_retries=3)

def share(file_id, email, role):
    """role: reader, commenter or writer. Google emails them a link."""
    service().permissions().create(fileId=file_id, body={"type": "user", "role": role, "emailAddress": email},
                                   sendNotificationEmail=True, fields="id").execute(num_retries=3)
