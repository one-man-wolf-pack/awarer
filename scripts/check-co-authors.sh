#!/bin/sh
# Accept a commit message that declares no co-author, or exactly the accepted pair.
#
# Usage: check-co-authors.sh [commit-message-file]   (with no argument, reads stdin)
#
# Git decides what the final trailer block is; this file owns only the pair. Co-author
# key recognition is case-insensitive, so an altered key casing activates the rule and
# then fails it against the exact accepted lines.
set -eu

codex='Co-authored-by: Codex <noreply@openai.com>'
claude='Co-authored-by: Claude <noreply@anthropic.com>'

[ "$#" -le 1 ] || { echo "usage: check-co-authors.sh [commit-message-file]" >&2; exit 2; }
[ "$#" -eq 0 ] || exec < "$1"

# Git's status is checked before the selection rather than after it. A git that failed
# and a message that declares nothing both produce no output, and treating the first as
# the second would pass every commit in a repository where this check cannot run at all.
trailers=$(git interpret-trailers --parse)
declared=$(printf '%s\n' "$trailers" | grep -i '^co-authored-by:' || true)
[ -n "$declared" ] || exit 0

if [ "$(printf '%s\n' "$declared" | sort)" = "$(printf '%s\n%s\n' "$codex" "$claude" | sort)" ]; then
    exit 0
fi

printf 'this commit declares a co-author, so its co-author set must be exactly:\n  %s\n  %s\ndeclared:\n%s\ndeclaring none at all is equally valid.\n' \
    "$codex" "$claude" "$(printf '%s\n' "$declared" | sed 's/^/  /')" >&2
exit 1
