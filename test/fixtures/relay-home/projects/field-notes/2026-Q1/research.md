# Imaging pipeline — per-batch normalization

## Why change

Per-frame normalization makes frames internally consistent but loses the
absolute-intensity signal across a session. Three of our analyses depend
on relative intensity between frames in the same batch.

## Proposal

1. Compute the normalization constant once per batch (the median of the
   middle 60% of pixels across all frames in the batch).
2. Apply it uniformly to every frame in the batch.
3. Cache the constant alongside the batch metadata so re-processing is
   deterministic.

## Risks

- Outlier batches (e.g., the operator left the lamp on) will skew the
  constant. Mitigate with a sanity-check threshold and a fallback to
  per-frame when the median falls outside `[0.4, 0.9]`.
- Existing analyses that assume per-frame normalization will need to be
  re-run. List in the next group meeting.

## Sources

- 2024 internal report (vault: `2024-Q3/imaging-recap.md`)
- Reference paper on batch-effects in intensity imaging
