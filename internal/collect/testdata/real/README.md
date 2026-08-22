# Captured from the target host

Real output, scrubbed by `capture-fixtures.sh` at the repo root: drive
serials, WWNs, the hostname and systemd machine/boot ids are replaced by
placeholders; byte counts and journal timestamps are left intact because
the parsers read them.

| file | produced by |
|---|---|
| `smart.jsonl` | `journalctl -t smartd -o json --no-pager -n 200` |
| `zed.jsonl` | `journalctl -t zed -o json --no-pager -n 200` |
| `kernel.jsonl` | `journalctl -k -p 3 -o json --no-pager -n 200` |
| `../sensors-hwmon.json` | `sensors -j` |

The file names match what the hermetic `journalctl` stub in `../bin` serves
for each filter combination, so a test copies them into a journal directory
and the collector reaches them through the normal path.
