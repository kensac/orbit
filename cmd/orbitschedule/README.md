# Compile a recurring schedule

`go run ./cmd/orbitschedule --timezone America/New_York --anchor 2026-11-02T00:00:00-05:00 --count 5 'every weekday at 9am'` prints an existing RRULE trigger and the next five fires strictly after the anchor. Without `--anchor`, the preview starts after the current instant. `--timezone` is required and is an IANA location or `UTC`; host-dependent `Local` is refused. The output is a preview and does not change an agent or enqueue work.

Supported recurrences, each followed by `at` and a time:

- `daily` or `every day`
- `every weekday` (Monday through Friday)
- `every monday` through `every sunday` (full names)
- `every other monday` through `every other sunday`
- `the 1st of every month` through `the 31st of every month`
- `every month on the 1st` through `every month on the 31st`

Times must be unambiguous: `09:00` and `23:30` use the 24-hour clock; `9am`, `9:30pm`, `12am` and `12pm` use explicit meridiems. Bare `8`, `9:00`, abbreviations, timezone names embedded in the phrase, free-form prose and unsupported recurrences are errors. Case and whitespace are normalized. Dates such as `11st` are refused.

The emitted trigger's `start` is the anchor's local date at midnight. An every-other-week rule uses the Monday-based week containing that date as its first eligible week, and repeats every second week. A requested weekday earlier in that first week is before the start and is omitted. Monthly dates missing from a month are skipped rather than moved to another date. The anchor is not a first-fire guarantee: the preview lists only fires strictly after its instant.

Set the target agent's `timezone` to the output's `timezone`, and copy the `trigger` object into the agent's `triggers` array. A trigger does not carry its own timezone; changing the agent's timezone changes its recurrence. The compiler validates the trigger using `spec.TriggerSchedule`, the same path configuration loading uses. Quiet hours, calendar-aware skipping, applying the trigger and parsing arbitrary natural language are separate capabilities.

The library interface is `scheduletext.Parse(text, anchor, loc) (spec.Trigger, error)`. It requires a nonzero anchor and an explicit location. A local date whose midnight does not exist is refused, with an error asking for another anchor date.
