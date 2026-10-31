#!/usr/bin/env bash
# Mutation testing of the write-rule core (internal/core).
#   scripts/mutation.sh            # gremlins on ./internal/core, then ~25 hand-picked flips of validate.go
#   OPENETL_TEST_DSN=... scripts/mutation.sh   # the hand-picked pass also runs ./internal/store (differential test)
# gremlins is installed with `go install` into $GOBIN (default: $TMPDIR/openetl-mutation-bin). The hand-picked pass edits a COPY of validate.go in a scratch
# worktree, never the real file. A mutant is "killed" when `go test` fails.
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)
BIN="${GOBIN:-${TMPDIR:-/tmp}/openetl-mutation-bin}"
mkdir -p "$BIN"
if [ ! -x "$BIN/gremlins" ]; then
  GOBIN="$BIN" go install github.com/go-gremlins/gremlins/cmd/gremlins@latest || echo "gremlins install failed; running the hand-picked pass only"
fi
if [ -x "$BIN/gremlins" ]; then
  echo "== gremlins ./internal/core"
  "$BIN/gremlins" unleash ./internal/core --workers 2 --timeout-coefficient 6 | tail -n 6
fi

echo "== hand-picked operator flips in internal/core/validate.go"
SCRATCH=$(mktemp -d)
trap 'rm -rf "$SCRATCH"' EXIT
mkdir "$SCRATCH/repo"
tar --exclude=.git -cf - . | tar -xf - -C "$SCRATCH/repo"
cd "$SCRATCH/repo"
F=internal/core/validate.go
cp "$F" "$SCRATCH/orig.go"
PKGS="./internal/core"
[ -n "${OPENETL_TEST_DSN:-}" ] && PKGS="./internal/core ./internal/store"

# from|||to  (first occurrence only)
MUTANTS=(
'st != StatePass && st != StateNA|||st != StatePass || st != StateNA'
'if s.Seq >= seq {|||if s.Seq <= seq {'
'case StatePass:|||case StateOpen:'
'reason.Kind != KindReject) {|||reason.Kind == KindReject) {'
'reason.Kind != KindNotApplicable|||reason.Kind == KindNotApplicable'
'if ov == StatePass { // leaving PASSED|||if ov != StatePass { // leaving PASSED'
'reason.Kind != KindDiscard|||reason.Kind == KindDiscard'
'reason.Key == "merged"|||reason.Key != "merged"'
'target <= 0|||target < 0'
'target == in.EntityID {|||target != in.EntityID {'
'in.EntityID != 0 \&\& target|||in.EntityID == 0 \&\& target'
'if !exists {|||if exists {'
'if !ok {\n\t\t\treturn nil|||if ok {\n\t\t\treturn nil'
'next == in.EntityID|||next != in.EntityID'
'in.Actor.Channel == ChannelLegacy ||||||in.Actor.Channel != ChannelLegacy ||'
'!r.Active|||r.Active'
'r.NoteRequired \&\& strings.TrimSpace(in.Note) == ""|||r.NoteRequired \&\& strings.TrimSpace(in.Note) != ""'
'r.ToValue == nil \|\| \*r.ToValue == nv|||r.ToValue != nil \|\| *r.ToValue == nv'
'r.Side == SideAny \|\| r.Side == in.Actor.Side \|\| in.Actor.Role == RoleAdmin|||r.Side == SideAny \|\| r.Side == in.Actor.Side'
'r.Side == SideAny \|\| r.Side == in.Actor.Side|||r.Side == SideAny \|\| r.Side != in.Actor.Side'
'r.ReasonKind == nil \|\| (reason != nil|||r.ReasonKind != nil \|\| (reason != nil'
'reason.Kind == \*r.ReasonKind|||reason.Kind != *r.ReasonKind'
'len(byKey) == 0|||len(byKey) != 0'
'len(byValue) == 0|||len(byValue) != 0'
'len(bySide) == 0|||len(bySide) != 0'
'c.Key == "into"|||c.Key != "into"'
'seq == 0|||seq != 0'
)
killed=0; survived=0; total=0
for m in "${MUTANTS[@]}"; do
  from="${m%%|||*}"; to="${m##*|||}"
  cp "$SCRATCH/orig.go" "$F"
  FROM="$from" TO="$to" perl -0pi -e 'BEGIN{$f=$ENV{FROM}; $t=$ENV{TO}; $f=~s/\\&/&/g; $t=~s/\\&/&/g; $f=~s/\\\|/|/g; $t=~s/\\\|/|/g; $f=~s/\\\*/*/g; $t=~s/\\\*/*/g; $f=~s/\\n/\n/g; $f=~s/\\t/\t/g; $t=~s/\\n/\n/g; $t=~s/\\t/\t/g;} s/\Q$f\E/$t/' "$F"
  if cmp -s "$F" "$SCRATCH/orig.go"; then echo "  (pattern not found, fix the script) $from"; continue; fi
  total=$((total+1))
  if go test -count=1 -p 1 $PKGS >/dev/null 2>&1; then
    survived=$((survived+1)); echo "  SURVIVED: $from  ->  $to"
  else
    killed=$((killed+1))
  fi
done
echo "hand-picked: killed $killed of $total (survivors $survived)"
[ "$survived" -eq 0 ]
