"""One-time Google sign-in for the bot's Drive access, as ieeecsucf@gmail.com.

Opens a browser to sign in and approve, then prints the saved login (a refresh
token) to stdout, so pipe it straight to the server instead of saving it here:

    uv run --with google-auth-oauthlib python scripts/drive_login.py client_secret.json \\
      | ssh -i ~/ssh-key-2025-01-03.key ubuntu@158.101.101.238 \\
        'sudo tee /home/ubuntu/ieee-cs-bot/credentials/drive_user.json >/dev/null && sudo systemctl restart ieee-bot'

client_secret.json is the "Desktop app" OAuth client downloaded from the bot's
Google Cloud project (APIs & Services → Credentials).
"""
import sys

from google_auth_oauthlib.flow import InstalledAppFlow

SCOPES = ["https://www.googleapis.com/auth/drive"]

if len(sys.argv) != 2:
    sys.exit(__doc__)
print("A browser window will open. Sign in as ieeecsucf@gmail.com and approve.", file=sys.stderr)
creds = InstalledAppFlow.from_client_secrets_file(sys.argv[1], SCOPES).run_local_server(port=0, prompt="consent")
print(creds.to_json())
print("Done. The login went to stdout (the pipe), not the screen.", file=sys.stderr)
