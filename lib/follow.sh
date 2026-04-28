#!/usr/bin/env bash
# follow.sh — live-tail a transcript JSONL and pretty-print each entry.
# Fully read-only. Intended to run as the foreground process of the bottom
# tmux pane when the user hits `f` on a card.

follow_session() {
  local transcript="${1:-}"
  if [[ -z "$transcript" || ! -f "$transcript" ]]; then
    printf 'agent-monitor: transcript not found: %s\n' "$transcript" >&2
    return 1
  fi

  local banner
  banner="$(basename "$transcript" .jsonl)"
  printf '\033[36m── follow: %s ──  (read-only tail; press "d" in dashboard to return)\033[0m\n' "$banner"

  tail -n 30 -F "$transcript" 2>/dev/null \
    | while IFS= read -r line; do
        [[ "$line" == '{'* ]] || continue
        jq -r --arg now "$(date '+%H:%M:%S')" '
          def trunc(n): if (. | length) > n then (.[0:n] + "…") else . end;
          def oneline: gsub("[\n\r\t]+"; " ");
          if .role == "assistant" then
            (.message.content // [])
            | map(
                if .type == "tool_use" then
                  "\u001b[33mtool\u001b[0m  " + .name + "  " + ((.input // {}) | tostring | oneline | trunc(100))
                elif .type == "text" then
                  "\u001b[37mtext\u001b[0m  " + (.text | oneline | trunc(160))
                else empty end
              )
            | map($now + "  " + .)
            | .[]
          elif .role == "user" then
            $now + "  \u001b[35muser\u001b[0m  " + ((.message.content // "") | tostring | oneline | trunc(160))
          else empty end
        ' <<<"$line" 2>/dev/null
      done
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  follow_session "$@"
fi
