# Rollout modes and counterfactual decisions

`spec.mode` controls whether HPA+ observes or changes its workload's replica count. Install a mode-aware operator
image **and its matching CRD** before using this field. An older CRD can prune unknown fields; an older binary can
ignore a newer field and continue scaling. Confirm the installed image and the stored `spec.mode`, then verify the
current-generation decision report. Do not downgrade to a binary that predates rollout modes while relying on observe.

| Mode | Behavior |
| --- | --- |
| `observe` | Gather metrics, sample history and run models. Report reactive/control counterfactuals, but never issue a new Scale write or add simulated recommendations/events to real accounting. |
| `assist` | Select using the configured model strategy, then floor the selected recommendation at the raw reactive baseline. Apply the normal partial-metric guard, stabilization, rate limits, disabled directions and min/max bounds. |
| `control` | Preserve existing `decisionType`/`includeHPA` behavior. If no model is usable, use the reactive baseline. |
| omitted or API-pruned `null` | `control`, preserving existing v1alpha1 installations. Explicitly set `observe` for a new trial. An empty string is invalid. |

Assist is a baseline floor, not an ensemble average or a promise to provision every predicted replica. It can predict
an earlier/larger upscale and can delay a downscale; it cannot select a smaller raw recommendation than the baseline.
Normal behavior still limits the result, so a disabled upscale or `maxReplicas` can prevent reaching even that baseline.
There is no automatic promotion, confidence gate or arbitrary model weighting. Reactive fallback remains fixed when
all models are unavailable; control does not introduce an unconfigured zero-replica fallback.

## Start with observe

Use the resource shape in [getting started](getting-started.md) with `spec.mode: observe`. Set the target, bounds and
training window for your workload, and configure positive CPU requests. Observation needs the usual
metric/history permissions and resources: it executes forecasts and writes history/status, despite not writing Scale.
The shared operator RBAC still grants Scale access for resources in assist/control; observe is a controller policy, not
a separate permission boundary.

During observation, a native HPA or another established mechanism may remain the sole active scaling controller. HPA+
reports what its plans would have recommended for the **actual** observed replica count, samples and accounting history.
It does not simulate how alternative replica counts would have changed subsequent CPU measurements, nor maintain
parallel counterfactual stabilization histories. This is decision evidence, not a forecast-quality benchmark or causal
claim about production savings.

Check the current object's `status.observedGeneration`, `status.lastDecision.observedGeneration`, timestamps, metric
availability and model reports. A stale or missing report is not evidence that the current spec has been evaluated.
The `ScalingActive=False/ObserveOnly` condition is expected, and does not emit a failure/recovery Event merely because
you changed rollout mode. Sampling/model failures still report their usual health conditions.

## Promote deliberately

1. Observe through representative peaks and quiet periods. Allow enough distinct CPU samples to warm each model and
   compare baseline/control recommendations with actual workload behavior. Review fallback, prediction age and latency.
2. Before enabling assist/control, arrange a single active scaling writer. Disable or remove the other controller through
   your normal workload procedure; HPA+ does not take over or disable native HPAs automatically.
3. Set `mode: assist` explicitly with conservative min/max and scaling policies. Verify current-generation reports,
   accepted-write accounting, worker budgets and provider availability before widening the bounds.
4. Use `mode: control` only after reviewing the chosen `decisionType`, `includeHPA` and unavailable-model behavior.
   Omitting mode also selects control for backward compatibility; explicit mode makes the rollout intent reviewable.

A mode edit increments generation. In-flight fits/decisions are rejected when the controller sees a changed generation,
including rechecks before and after publishing a scale journal. Kubernetes does not provide an atomic transaction
between a spec edit and a Scale write: a write that already passed the final check can still complete concurrently.
Switching to observe is not a rollback of an already accepted change. Verify the latest generation's observe report
and inspect pending accounting before assuming the old cycle has drained.

An unresolved journal from a previous assist/control attempt is reconciled conservatively before another cycle,
even after a switch to observe. Recovery can update accounting and clear a resolved journal; it never replays Scale.
Existing real stabilization/event histories survive mode changes and expire under their usual policies. Observe
bypasses the previous scale cooldown for reporting, and never advances that cooldown itself. Target identity changes
retain their existing reset rules.

## Latest decision report

`status.lastDecision` is one bounded report, replaced on each evaluated plan and cleared when a new generation has
not produced a plan. It uses the same 256 KiB complete-status budget and optimistic ownership checks as other status.
It is best effort, not the durable scale journal. Status publication failure can leave older evidence behind.

| Field | Meaning |
| --- | --- |
| `id`, `evaluatedAt`, `observedGeneration`, `targetUID` | Correlation and snapshot identity; ID also appears in logs and, for scale changes, the journal. |
| `mode` | Rollout policy: observe, assist or control. |
| `candidateMode` | Separate candidate composition: baseline, predictive or mixed. |
| `requestedReplicas`, `observedReplicas` | Scale spec count and observed pod count at evaluation. These may differ. |
| `baselineReplicas` | Raw reactive recommendation. |
| `baselineDesiredReplicas` | Reactive-only plan after safety/behavior constraints using actual accounting history. |
| `controlDesiredReplicas` | Configured control plan, including `decisionType`/`includeHPA`, after the same safety constraints. Per-model raw candidates remain in `modelStatuses` and logs. |
| `desiredReplicas` | Assist/control's planned request; in observe, the sampled Scale spec count, which HPA+ leaves unchanged. |
| `scaleWrite` | `observed`, `unchanged`, `accepted`, `rejected`, `unknown` or `not_attempted`. Acceptance can be followed by accounting failure; inspect the journal and health. |

Model failure/backoff/cache and source/prediction expiry retain their existing checks. Failed or expired cycles can
still have diagnostic plan evidence with `not_attempted`; they do not become successfully completed observations.
No report authorizes a later Scale write. The report's counterfactual numbers must not be inserted into real safety histories.

The `hpaplus_rollout_evaluations_total{mode}` metric counts successfully completed cycles in all three modes. Existing
`hpaplus_decisions_total` and decision-constraint metrics count only assist/control accounting, so observe cannot inflate
baseline-fallback alerts or actual scale statistics. Model/sampler telemetry still includes work done in observe mode.
The dashboard has a separate rollout panel; decision JSON includes `rolloutMode` and both counterfactual desired counts.
