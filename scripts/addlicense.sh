#!/usr/bin/env bash
# Add the Apache 2.0 header to every .go file that lacks one. Idempotent.
#
#   ./scripts/addlicense.sh          # apply
#   ./scripts/addlicense.sh --check  # fail if any file is missing it (for CI)
set -euo pipefail

CHECK=false
[ "${1:-}" = "--check" ] && CHECK=true

read -r -d '' HEADER <<'HDR' || true
// Copyright 2026 Autometiq
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
HDR

missing=0
while IFS= read -r f; do
  if head -3 "$f" | grep -q 'Licensed under the Apache License'; then
    continue
  fi
  missing=$((missing + 1))
  if $CHECK; then
    echo "missing license header: $f"
    continue
  fi
  # The header goes ABOVE the package doc comment with a blank line between.
  # Without that blank line Go treats the licence text as the package's
  # documentation and it renders on pkg.go.dev instead of the real doc comment.
  tmp=$(mktemp)
  { printf '%s\n\n' "$HEADER"; cat "$f"; } > "$tmp"
  mv "$tmp" "$f"
  echo "added: $f"
done < <(find . -name '*.go' -not -path './vendor/*' -not -path '*/node_modules/*')

if $CHECK && [ "$missing" -gt 0 ]; then
  echo "$missing file(s) missing the licence header; run ./scripts/addlicense.sh"
  exit 1
fi
echo "done ($missing file(s) changed)"
