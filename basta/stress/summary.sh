#!/bin/bash
# Turn run.sh's <label>.env files into a markdown table; flags any v2-* arm that lost builds or crashed.
# usage: summary.sh <out-dir> [unit-test-status-file]; writes <out-dir>/summary.md
set -euo pipefail
out=$1 unit=${2:-}
bad=()
{
echo "| arm | driver result | lost | panic: | failed to get edge | merged input vertex not found | evaluating released result | daemon alive (exit) |"
echo "|---|---|---|---|---|---|---|---|"
for f in "$out"/*.env; do
  unset label result panics failed_get_edge merged_not_found released running exit_code
  while IFS='=' read -r k v; do printf -v "$k" '%s' "$v"; done < "$f"
  case $result in
    *lost=*) lost=$(sed -E 's/.* lost=([0-9]+).*/\1/' <<<"$result") ;;
    RESULT:*" of "*) lost=$(sed -E 's/^RESULT: ([0-9]+) of.*/\1/' <<<"$result") ;;
    *) lost="?" ;;
  esac
  alive=$running; [ "$running" = true ] || alive="**no ($exit_code)**"
  echo "| $label | \`${result#RESULT: }\` | $lost | $panics | $failed_get_edge | $merged_not_found | $released | $alive |"
  if [[ $label == v2-* ]] && { [ "$lost" != 0 ] || [ "$panics" != 0 ] || [ "$running" != true ]; }; then
    bad+=("$label: lost=$lost panics=$panics alive=$running")
  fi
done
if [ -n "$unit" ] && [ -f "$unit" ]; then
  echo; echo "**Unit tests**"; echo; cat "$unit"
fi
echo
if [ ${#bad[@]} -gt 0 ]; then
  echo "**V2 REGRESSION:** ${bad[*]}"
else
  echo "**v2: no lost builds, no panics, daemon alive in every arm.**"
fi
} > "$out/summary.md"
[ ${#bad[@]} -eq 0 ] || for b in "${bad[@]}"; do echo "::error title=v2 lost builds or crashed::$b"; done
