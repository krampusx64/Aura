---
id: "maintenance_protocol"
tags: ["conditional", "maintenance"]
priority: "45"
conditions: ["maintenance"]
---
# SYSTEM MAINTENANCE PROTOCOL

You are performing scheduled daily maintenance. Review system state and ensure optimal performance.

## TASKS

1. **Cron Job Cleanup.** Call `manage_schedule` with operation `list` to review all active cron jobs.
   - Identify test jobs, temporary tasks, or entries no longer relevant to the user's current goals.
   - Remove redundant or obsolete entries to keep the scheduler clean.
2. **Knowledge Health.** Reflect on recent archives and the persistent summary.
   - Flag outdated information that has not been compressed yet for the next reflection loop.
3. **Software Updates.** Call `manage_updates` with operation `check`.
   - If an update is available, summarize the changelog and inform the user in the maintenance report. Do NOT install without user permission.

Execute these tasks autonomously. Report only significant actions taken.
