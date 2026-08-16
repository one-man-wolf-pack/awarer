#!/bin/sh
# Render the Homebrew formula template with one already validated release identity.
#
# Usage: render-formula.sh <tag> <revision> <template> <output>
#
# The template is the complete formula and this is pure substitution: it never parses Ruby,
# reads the tap's current formula, compares versions, or adopts remote state. Every way the
# substitution can silently produce a wrong formula is refused instead — an argument that is
# not a release identity, a template that lost a placeholder, and a rendered file that still
# carries one. Failing here fails the release job loudly, which is the accepted recovery.
set -eu

tag_marker='@@AWARER_TAG@@'
revision_marker='@@AWARER_REVISION@@'

[ "$#" -eq 4 ] || { echo "usage: render-formula.sh <tag> <revision> <template> <output>" >&2; exit 2; }

tag=$1
revision=$2
template=$3
output=$4

fail() { printf 'render-formula: %s\n' "$1" >&2; exit 1; }

# The release workflow proves the tag and its commit before this runs. Re-checking their shape
# costs nothing and stops an empty expansion from rendering a formula that builds "".
printf '%s' "$tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
    fail "tag $tag is not a strict stable vMAJOR.MINOR.PATCH release tag"
case $revision in
    *[!0-9a-f]* | "") fail "revision $revision is not a full lowercase hexadecimal commit sha" ;;
esac
[ "${#revision}" -eq 40 ] || fail "revision $revision is not 40 characters"

[ -s "$template" ] || fail "template $template is missing or empty"
grep -qF "$tag_marker" "$template" || fail "template $template has no $tag_marker placeholder"
grep -qF "$revision_marker" "$template" || fail "template $template has no $revision_marker placeholder"

rendered=$(sed -e "s|$tag_marker|$tag|g" -e "s|$revision_marker|$revision|g" "$template")

[ -n "$rendered" ] || fail "rendering $template produced an empty formula"
# Any remaining marker means the template grew a placeholder this renderer does not know, so
# the file would reach the tap with a literal @@...@@ in it.
case $rendered in
    *@@*) fail "rendered formula still carries a placeholder; template $template is ahead of this renderer" ;;
esac

printf '%s\n' "$rendered" > "$output"
