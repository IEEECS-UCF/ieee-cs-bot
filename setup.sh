#!/usr/bin/env bash

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  echo >&2 "Error: To activate the environment, this script must be sourced."
  echo >&2 "Run it using: source $0"
  exit 1
fi

_setup_bot_env() {
  local venv_dir="venv"
  local req_file="requirements.txt"

  if ! command -v python3 >/dev/null 2>&1; then
    echo >&2 "Error: Python 3 is required but not installed or not in PATH."
    return 1
  fi

  if [ ! -d "$venv_dir" ]; then
    python3 -m venv "$venv_dir"
  fi

  . "$venv_dir/bin/activate"

  pip install --upgrade pip --quiet

  if [ -f "$req_file" ]; then
    pip install -r "$req_file" --quiet
  else
    pip install discord.py python-dotenv --quiet
    pip freeze >"$req_file"
  fi
}

_setup_bot_env

unset -f _setup_bot_env
