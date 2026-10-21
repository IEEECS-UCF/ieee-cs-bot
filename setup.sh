#!/usr/bin/env bash

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  echo >&2 "Error: To activate the environment, this script must be sourced."
  echo >&2 "Run it using: source $0"
  exit 1
fi

if ! command -v uv >/dev/null 2>&1; then
  echo >&2 "Error: uv is required (https://docs.astral.sh/uv/getting-started/installation/)."
  return 1
fi

uv sync && . .venv/bin/activate
