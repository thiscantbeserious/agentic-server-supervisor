# SOP: Routine Health Check

## Objective
Perform a standard health check of the server and identify any anomalies.

## Steps
1. **Check Resources:**
   - Run `df -h` to check disk space.
   - Run `free -m` to check memory usage.
   - Run `uptime` to check load average.
2. **Scan Logs:**
   - Check `/etc/gemini-watcher/system/var/log/` for recent errors or warnings.
3. **Analyze & Report:**
   - Correlate any findings with previous ticks.
   - Provide a status update according to the "Output Format" in AGENTS.md.
