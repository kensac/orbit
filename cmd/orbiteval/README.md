# Saved artifact evaluations

`go run ./cmd/orbiteval --suite cmd/orbiteval/testdata/suite.json --results cmd/orbiteval/testdata/results.json` checks captured run outputs against deterministic reference properties and prints a JSON regression and cost report.

The command reads two local files, makes no network requests, fetches no sources, executes no backend and writes nothing except its report to stdout. It exits nonzero for regressions or invalid files. This checks saved artifacts; it does not replay a run against the current charter or skills, and does not establish that the supplied output was produced from the reference inputs.

A version 1 suite has named `cases`. Each case retains `provenance` (the reference run, charter/skills revision and capture context), `inputs` (the saved input JSON), `checks` and an optional nonnegative `max_cost_usd`. Keep credentials out of fixtures. Names must be unique and the result array must contain exactly one result per case. A missing result is a regression; duplicate and unexpected results are errors.

Each check has an RFC 6901 `pointer`, an `op` and a `value`. `equals` compares JSON values recursively with exact decimal number comparison; `contains` checks a string substring; `count` checks an array length against a nonnegative integer. An empty pointer selects the entire output. Missing fields differ from explicit `null`, and array indices cannot have leading zeros. Unknown operations, fields and malformed pointers are errors.

Each captured result has a matching `name`, an `output` JSON value and an optional nonnegative `cost_usd`. `known_cost_usd` is the exact decimal sum of costs supplied with the results; `missing_costs` counts absent costs, which are never assumed to be zero. A cost bound fails when the result has no reported cost. Reports contain check locations and failures, never the captured output or input contents.

To compare a changed configuration, capture its outputs separately under the same case names, then pass that result file to this command with the unchanged reference suite. Current-charter execution, automatic run export and model grading are separate capabilities.
