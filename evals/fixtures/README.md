# Synthetic fixtures

**Everything here is made up.** These files exist to test the scoring code before the lab
runs. They are not lab data, and no number derived from them may appear in `EVALS.md` or
anywhere else as a result.

- `faults/`: fault files F91–F98 in the fault-library schema (#23). Ids say `synthetic`; the
  real library (`chaos/faults/`) uses F01–F26. Their manifests do not exist.
- `recordings/synthetic-session.json`: one session in the recording format the live runner
  writes, marked `"synthetic": true`. Its runs cover every scoring path: a correct RCA with an
  executed fix and a second investigation whose unsafe proposal was never approved, a top-3-only
  RCA in a window that still burned, an unsafe action rejected at approval, a correct "do
  nothing", an unsafe action refused by the executor, a node fault answered as `node`, a false
  alarm with a disallowed action and leftover 6-hour burn, a failed investigation, a skipped run,
  a run the agent dropped for its daily budget, a ticket-only run, a run where no alert reached
  the agent, a settings mismatch, and a cost the list prices can't explain.
- `postmortems/`: postmortems in #20's template, for the human baseline and the "seen before?"
  split. Dates are in 1999-2000 so they can't be mistaken for lab incidents.
- `baselines/synthetic-other-tool.yaml`: the baseline file format for another tool.

Replay them with `make evals-replay` (writes to `evals/results/`).
