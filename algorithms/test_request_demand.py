"""Contract tests for request forecasting, including one actual XGBoost fit."""
import copy
from datetime import datetime, timezone
import unittest
from unittest.mock import patch

import numpy as np

import request_demand as demand


def stamp(value):
    return datetime.fromtimestamp(value, timezone.utc).isoformat().replace("+00:00", "Z")


def payload(count=60):
    start = 1800000000
    observations = []
    for index in range(count):
        at = start + index * 30
        observations.append(dict(at=stamp(at), until=stamp(at + 30), receivedAt=stamp(at + 31),
                                 arrivalsPerSecond=float(30 + 20 * np.sin(index * 2 * np.pi / 10)), features={}))
    return dict(protocol=demand.PROTOCOL, requestedAt=stamp(start + count * 30 + 2),
                firstForecastAt=stamp(start + count * 30 + 30),
                config=dict(intervalSeconds=30, historySize=60, lags=12, horizonSteps=3,
                            quantile=.95, seasonalFeatures=True, nEstimators=300, maxDepth=5, features=[]),
                observations=observations)


def windowed_payload():
    data = payload()
    end = demand.timestamp(data["observations"][-1]["until"])
    data["requestedAt"] = demand.stamp(end+4)
    data["config"]["features"] = [dict(name="cpu_usage", unit="cores", maxAgeSeconds=90)]
    for index, observation in enumerate(data["observations"]):
        until = demand.timestamp(observation["until"])
        observation["receivedAt"] = demand.stamp(until+4)
        observation["features"]["cpu_usage"] = dict(value=1.+index/100, unit="cores",
            sourceAt=demand.stamp(until+2), receivedAt=demand.stamp(until+4),
            measurementWindow={"from": observation["at"], "to": observation["until"]})
    return data


class MeanRegressor:
    def __init__(self, **kwargs):
        self.params = kwargs

    def fit(self, x, y):
        self.mean = np.mean(y)

    def predict(self, rows):
        return np.full(len(rows), self.mean)


class RequestDemandTests(unittest.TestCase):
    def test_finer_forecasts_keep_thirty_second_histories_and_real_labels(self):
        data = payload()
        data["config"].update(forecastIntervalSeconds=10, horizonSteps=9)
        for row in data["observations"]:
            at = demand.timestamp(row["at"])
            row["arrivalIntervals"] = [dict(at=demand.stamp(at+i*10), until=demand.stamp(at+(i+1)*10),
                receivedAt=row["receivedAt"], rate=row["arrivalsPerSecond"]*factor, coverage=1., boundaryAveraged=True)
                for i, factor in enumerate((.5, 2., .5))]
        data["firstForecastAt"] = demand.stamp(demand.ceiling_interval(demand.timestamp(data["requestedAt"]), 10))
        parsed = demand.parse_request(data)
        matrices = demand.build_matrices(parsed)[0]
        self.assertIsNotNone(matrices)
        self.assertEqual(60, len(parsed["observations"]))
        self.assertEqual(180, len(parsed["fine_targets"]))
        self.assertEqual(30, parsed["interval"])
        self.assertEqual(10, parsed["forecast_interval"])
        actual_labels = {float(p["rate"]) for p in parsed["fine_targets"].values()}
        self.assertTrue(set(matrices[1]).issubset(actual_labels))
        result = demand.predict(data, MeanRegressor)
        self.assertEqual(9, len(result["forecasts"]))
        self.assertEqual(10, result["diagnostics"]["forecastIntervalSeconds"])
        actual = demand.predict(data)  # Exercise the real direct XGBoost fit.
        self.assertEqual("ready", actual["status"])
        self.assertEqual(9, len(actual["forecasts"]))
        for i, point in enumerate(result["forecasts"]):
            self.assertEqual(parsed["first"]+i*10, demand.timestamp(point["at"]))
            self.assertEqual(10, demand.timestamp(point["until"])-demand.timestamp(point["at"]))
        row = data["observations"][-1]
        row["arrivalIntervals"][0].update(rate=None, coverage=.8)
        parsed = demand.parse_request(data)
        self.assertIsNone(parsed["fine_targets"][demand.timestamp(row["at"])]["rate"])
        row["arrivalIntervals"][0]["rate"] = 0.
        with self.assertRaisesRegex(ValueError, "95%"):
            demand.parse_request(data)

    def test_fine_subinterval_receipts_and_scope_are_validated(self):
        data = payload()
        data["config"].update(forecastIntervalSeconds=10, horizonSteps=9)
        row = data["observations"][-1]
        point = dict(at=row["at"], until=demand.stamp(demand.timestamp(row["at"])+10),
                     receivedAt=demand.stamp(demand.timestamp(row["receivedAt"])+1), rate=1., coverage=1.)
        row["arrivalIntervals"] = [point]
        with self.assertRaisesRegex(ValueError, "backdated"):
            demand.parse_request(data)
        point["receivedAt"] = row["receivedAt"]
        row["arrivalIntervals"].append(dict(point))
        with self.assertRaisesRegex(ValueError, "duplicate"):
            demand.parse_request(data)

    def test_metadata_cache_does_not_cache_historical_availability(self):
        data = windowed_payload()
        parsed = demand.parse_request(data)
        item = parsed["observations"][-1]
        receipt = demand.timestamp(item["features"]["cpu_usage"]["receivedAt"])
        cache = {}
        with patch.object(demand, "feature_metadata", wraps=demand.feature_metadata) as read:
            early = demand.feature_window([item], parsed["specs"], receipt-1, cache)
            late = demand.feature_window([item], parsed["specs"], receipt, cache)
            earlier_again = demand.feature_window([item], parsed["specs"], receipt-1, cache)
        self.assertEqual(1, read.call_count)
        self.assertTrue(np.isnan(early[0]))
        self.assertEqual(1., early[1])
        self.assertFalse(np.isnan(late[0]))
        self.assertEqual(0., late[1])
        self.assertTrue(np.isnan(earlier_again[0]))

    def test_metadata_cache_matches_uncached_matrices_and_is_fit_local(self):
        data = windowed_payload()
        data["observations"][20]["features"]["cpu_usage"]["value"] = None
        parsed = demand.parse_request(data)
        with patch.object(demand, "feature_metadata", wraps=demand.feature_metadata) as read:
            cached = demand.build_matrices(parsed)[0]
        self.assertLessEqual(read.call_count, len(parsed["observations"]))
        original = demand.feature_window
        def uncached(items, specs, cutoff, _cache=None):
            return original(items, specs, cutoff)
        with patch.object(demand, "feature_window", side_effect=uncached):
            reference = demand.build_matrices(parsed)[0]
        for actual, expected in zip(cached[:3], reference[:3]):
            np.testing.assert_array_equal(actual, expected)
        changed = copy.deepcopy(data)
        changed["observations"][-1]["features"]["cpu_usage"]["value"] = 900.
        future = demand.build_matrices(demand.parse_request(changed))[0][2]
        self.assertFalse(np.array_equal(cached[2], future, equal_nan=True))

    def test_fit_shares_one_past_derived_period_and_retained_origin(self):
        data = payload(66)
        data["observations"][-4]["arrivalsPerSecond"] = None
        parsed = demand.parse_request(data)
        genuine = [item for item in parsed["observations"] if item["rate"] is not None]
        expected = float(genuine[-60]["at"])
        origins, periods, last_past = [], [], []
        original = demand.phase_features
        def varying(times, values):
            last_past.append(times[-1])
            return 300.+len(values)%3
        def record(times, origin, period):
            origins.append(origin); periods.append(period)
            return original(times, origin, period)
        with patch.object(demand, "estimate_period", side_effect=varying), patch.object(demand, "phase_features", side_effect=record):
            matrices = demand.build_matrices(parsed)[0]
        self.assertIsNotNone(matrices)
        self.assertEqual(1, len(last_past))
        self.assertEqual({300.}, set(periods))
        self.assertEqual({expected}, set(origins))
        self.assertLessEqual(expected, min(last_past))
        self.assertLessEqual(max(last_past), float(parsed["requested"]))
        self.assertGreater(expected, float(parsed["observations"][0]["at"]))  # rolling retention was actually exercised

    def test_seasonal_matrices_are_invariant_to_clock_translation(self):
        left = payload(66)
        right = copy.deepcopy(left)
        translation = 1800000030
        for key in ("requestedAt", "firstForecastAt"):
            right[key] = demand.stamp(demand.timestamp(right[key])+translation)
        for observation in right["observations"]:
            for key in ("at", "until", "receivedAt"):
                observation[key] = demand.stamp(demand.timestamp(observation[key])+translation)
        # Even a length-sensitive estimator must yield one coherent basis per fit.
        def varying(times, values):
            return 300.+len(values)%3
        with patch.object(demand, "estimate_period", side_effect=varying):
            a = demand.build_matrices(demand.parse_request(left))[0]
            b = demand.build_matrices(demand.parse_request(right))[0]
        for first, second in zip(a[:3], b[:3]):
            np.testing.assert_array_equal(first, second)

    def test_seasonal_basis_uses_genuine_native_targets_available_at_fit(self):
        data = payload(66)
        data["config"].update(forecastIntervalSeconds=10, horizonSteps=9)
        for observation in data["observations"]:
            at = demand.timestamp(observation["at"])
            observation["arrivalIntervals"] = [dict(at=demand.stamp(at+i*10),
                until=demand.stamp(at+(i+1)*10), receivedAt=observation["receivedAt"],
                rate=observation["arrivalsPerSecond"]+i-1, coverage=1) for i in range(3)]
        data["observations"][30]["arrivalIntervals"][0].update(rate=None, coverage=.94)
        future = copy.deepcopy(data["observations"][-1])
        future_at = demand.timestamp(future["until"])
        future.update(at=demand.stamp(future_at), until=demand.stamp(future_at+30),
                      receivedAt=demand.stamp(future_at+31), arrivalsPerSecond=999999.)
        future["arrivalIntervals"] = [dict(at=demand.stamp(future_at+i*10),
            until=demand.stamp(future_at+(i+1)*10), receivedAt=future["receivedAt"],
            rate=999999., coverage=1) for i in range(3)]
        data["observations"].append(future)
        before = copy.deepcopy(data)
        parsed = demand.parse_request(data)
        genuine = [r for r in parsed["observations"] if r["rate"] is not None]
        oldest = genuine[-60]["at"]
        expected = sorted((r for r in parsed["fine_targets"].values()
                           if r["at"] >= oldest and r["rate"] is not None), key=lambda r:r["at"])
        with patch.object(demand, "estimate_period", return_value=300.) as estimate:
            matrices, reason = demand.build_matrices(parsed)
        self.assertIsNotNone(matrices, reason)
        estimate.assert_called_once_with([float(r["at"]) for r in expected], [r["rate"] for r in expected])
        self.assertEqual(179, len(expected))
        self.assertNotIn(999999., estimate.call_args.args[1])
        self.assertEqual(9, len(matrices[2]))
        self.assertEqual(before, data)

    def test_fine_forecasts_do_not_subdivide_mean_labels_when_native_labels_absent(self):
        data = payload()
        data["config"].update(forecastIntervalSeconds=10, horizonSteps=9)
        matrices, reason = demand.build_matrices(demand.parse_request(data))
        self.assertIsNone(matrices)
        self.assertIn("insufficient causal direct training pairs", reason)

    def test_seasonal_origin_does_not_change_target_pairs_or_observed_inputs(self):
        data = windowed_payload()
        data["observations"][15]["features"]["cpu_usage"]["value"] = None
        seasonal = demand.build_matrices(demand.parse_request(data))[0]
        data["config"]["seasonalFeatures"] = False
        ordinary = demand.build_matrices(demand.parse_request(data))[0]
        np.testing.assert_array_equal(seasonal[0][:, :-4], ordinary[0][:, :-1])
        np.testing.assert_array_equal(seasonal[0][:, -1], ordinary[0][:, -1])
        np.testing.assert_array_equal(seasonal[1], ordinary[1])
        np.testing.assert_array_equal(seasonal[2][:, :-4], ordinary[2][:, :-1])
        np.testing.assert_array_equal(seasonal[2][:, -1], ordinary[2][:, -1])

    def test_native_counter_read_after_boundary_is_valid_actual_cpu_input(self):
        data = windowed_payload()
        result = demand.predict(data)  # actual XGBoost fit, not only DTO acceptance
        self.assertEqual("ready", result["status"])
        availability = result["diagnostics"]["featureAvailability"]["cpu_usage"]
        self.assertGreater(availability["trainingNonMissing"], 0)
        self.assertEqual(36, availability["inferenceTotal"])
        self.assertEqual(36, availability["inferenceNonMissing"])
        matrices = demand.build_matrices(demand.parse_request(data))[0]
        self.assertTrue((matrices[2][:, 51:85:3] == 0).all())  # age from measurement end, not future native read

    def test_windowed_late_feature_never_becomes_retroactively_available(self):
        data = windowed_payload()
        for observation in data["observations"]:
            until = demand.timestamp(observation["until"])
            observation["features"]["cpu_usage"]["receivedAt"] = demand.stamp(until+12)
        x, _, future, _, _ = demand.build_matrices(demand.parse_request(data))[0]
        self.assertTrue(np.isnan(x[:, 82]).all())  # latest CPU lag unknown at each historical T+4 decision
        self.assertTrue(np.isnan(future[:, 82]).all())
        data["requestedAt"] = demand.stamp(demand.timestamp(data["requestedAt"])+8)
        later_x, _, later_future, _, _ = demand.build_matrices(demand.parse_request(data))[0]
        np.testing.assert_array_equal(x, later_x)
        self.assertFalse(np.isnan(later_future[:, 82]).any())

    def test_windowed_missing_gaps_and_instant_future_read_are_not_filled(self):
        data = windowed_payload()
        data["observations"][-2]["features"]["cpu_usage"]["value"] = None
        config = demand.parse_request(data)
        future = demand.build_matrices(config)[0][2]
        self.assertTrue(np.isnan(future[:, 79]).all())
        self.assertTrue((future[:, 80] == 1).all())
        result = demand.predict(data, MeanRegressor)
        self.assertEqual(33, result["diagnostics"]["featureAvailability"]["cpu_usage"]["inferenceNonMissing"])
        # An instantaneous gauge cannot use the averaging-window exception.
        del data["observations"][-1]["features"]["cpu_usage"]["measurementWindow"]
        with self.assertRaisesRegex(ValueError, "instant feature"):
            demand.predict(data, MeanRegressor)

    def test_future_window_and_backdated_receipt_are_rejected(self):
        for kind in ("future-window", "receipt-before-source", "receipt-before-window-end"):
            data = windowed_payload()
            feature = data["observations"][-1]["features"]["cpu_usage"]
            until = demand.timestamp(data["observations"][-1]["until"])
            if kind == "future-window":
                feature["measurementWindow"]["to"] = demand.stamp(until+1)
            elif kind == "receipt-before-source":
                feature["receivedAt"] = demand.stamp(until+1)
            else:
                feature.update(sourceAt=demand.stamp(until-2), receivedAt=demand.stamp(until-1))
            with self.assertRaises(ValueError):
                demand.predict(data, MeanRegressor)

    def test_feature_age_uses_measurement_end_and_diagnostics_count_values(self):
        data = windowed_payload()
        for observation in data["observations"]:
            until = demand.timestamp(observation["until"])
            observation["features"]["cpu_usage"]["measurementWindow"] = dict(
                **{"from": demand.stamp(until-121), "to": demand.stamp(until-91)})
        result = demand.predict(data, MeanRegressor)
        actual = result["diagnostics"]["featureAvailability"]["cpu_usage"]
        self.assertEqual(0, actual["trainingNonMissing"])
        self.assertEqual(0, actual["inferenceNonMissing"])
        self.assertGreater(actual["trainingTotal"], 0)
        self.assertEqual(36, actual["inferenceTotal"])

    def test_configured_xgboost_parameters_reach_constructor_and_diagnostics(self):
        class RecordingRegressor(MeanRegressor):
            actual = None
            def __init__(self, **kwargs):
                type(self).actual = kwargs
                super().__init__(**kwargs)
        data = payload()
        selected = dict(learningRate=.03, subsample=.9, colsampleBytree=.7,
                        minChildWeight=0., gamma=.2, regLambda=2.5, regAlpha=.4)
        data["config"].update(selected)
        result = demand.predict(data, RecordingRegressor)
        for camel, snake in (("learningRate", "learning_rate"), ("subsample", "subsample"),
                ("colsampleBytree", "colsample_bytree"), ("minChildWeight", "min_child_weight"),
                ("gamma", "gamma"), ("regLambda", "reg_lambda"), ("regAlpha", "reg_alpha")):
            self.assertEqual(selected[camel], RecordingRegressor.actual[snake])
            self.assertEqual(selected[camel], result["diagnostics"][camel])
        self.assertEqual("reg:quantileerror", RecordingRegressor.actual["objective"])
        for name, invalid in (("learningRate", 0), ("subsample", 1.1), ("gamma", -1), ("regLambda", 1e6+1)):
            data["config"][name] = invalid
            with self.assertRaises(ValueError):
                demand.parse_request(data)
            data["config"][name] = selected[name]

    def test_arrival_coverage_metadata_does_not_turn_partial_data_into_zero(self):
        data = payload()
        data["observations"][-1].update(arrivalsPerSecond=0., arrivalCoverage=.94, arrivalBoundaryAveraged=True)
        with self.assertRaisesRegex(ValueError, "95%"):
            demand.parse_request(data)
        data["observations"][-1]["arrivalsPerSecond"] = None
        self.assertEqual("warming", demand.predict(data, MeanRegressor)["status"])

    def test_genuine_warmup(self):
        result = demand.predict(payload(59), MeanRegressor)
        self.assertEqual("warming", result["status"])
        self.assertEqual([], result["forecasts"])
        self.assertIn("have 59", result["reason"])

    def test_publication_timestamp_preserves_nanoseconds(self):
        data = payload(59)
        data["requestedAt"] = data["requestedAt"].replace("Z", ".123456789Z")
        self.assertEqual(data["requestedAt"], demand.predict(data, MeanRegressor)["generatedAt"])

    def test_real_fit_and_interval_units(self):
        result = demand.predict(payload())
        self.assertEqual("ready", result["status"])
        self.assertEqual("requests_per_second", result["unit"])
        self.assertEqual(3, len(result["forecasts"]))
        self.assertEqual(300, result["diagnostics"]["nEstimators"])
        for point in result["forecasts"]:
            self.assertGreaterEqual(point["value"], 0)
            self.assertEqual(30, demand.timestamp(point["until"]) - demand.timestamp(point["at"]))

    def test_real_fit_accepts_explicit_missing_arrival_lag(self):
        data = payload(61)
        data["observations"][-2]["arrivalsPerSecond"] = None
        result = demand.predict(data)
        self.assertEqual("ready", result["status"])
        self.assertEqual(3, len(result["forecasts"]))

    def test_future_labels_and_features_do_not_leak(self):
        first = payload()
        second = copy.deepcopy(first)
        second["observations"].append(dict(at=stamp(1800001800), until=stamp(1800001830),
                                           receivedAt=stamp(1800001831), arrivalsPerSecond=1e9, features={}))
        left = demand.build_matrices(demand.parse_request(first))[0]
        right = demand.build_matrices(demand.parse_request(second))[0]
        for a, b in zip(left[:3], right[:3]):
            np.testing.assert_array_equal(a, b)

    def test_late_feature_receipt_masks_historical_anchor(self):
        data = payload()
        data["config"]["features"] = [dict(name="memory", unit="bytes", maxAgeSeconds=3600)]
        for index, observation in enumerate(data["observations"]):
            observation["features"]["memory"] = dict(value=float(index), unit="bytes",
                sourceAt=observation["until"], receivedAt=data["requestedAt"])
        x, _, future, _, _ = demand.build_matrices(demand.parse_request(data))[0]
        # 12 lags + 11 differences + mean/std + 12 masks + 12 ages =49.
        self.assertTrue(np.isnan(x[:, 49:85:3]).all())
        self.assertTrue((x[:, 50:85:3] == 1).all())
        self.assertFalse(np.isnan(future[:, 49:85:3]).any())
        self.assertTrue((future[:, 50:85:3] == 0).all())

    def test_missing_recent_arrival_not_forward_filled(self):
        data = payload(61)
        data["observations"][-2]["arrivalsPerSecond"] = None
        result = demand.predict(data, MeanRegressor)
        self.assertEqual("ready", result["status"])
        future = demand.build_matrices(demand.parse_request(data))[0][2]
        self.assertTrue(np.isnan(future[0, 10]))
        self.assertEqual(1, future[0, 25+10])
        # Many gaps exceed the supported information floor and stay unavailable.
        data = payload(70)
        for observation in data["observations"][-9:-2]:
            observation["arrivalsPerSecond"] = None
        result = demand.predict(data, MeanRegressor)
        self.assertEqual("warming", result["status"])
        self.assertIn("lag window", result["reason"])

    def test_fractional_receipt_never_forecasts_started_interval(self):
        data = payload()
        # A receipt one millisecond after a boundary must advance to the next
        # boundary. Decimal parsing keeps this distinct from the boundary.
        receipt = demand.timestamp(data["observations"][20]["until"]) + demand.timestamp("1970-01-01T00:00:00.001Z")
        self.assertGreater(demand.ceiling_interval(receipt, 30), receipt)

    def test_ninety_second_grid_matches_three_direct_target_starts(self):
        data = payload()
        config = demand.parse_request(data)
        x, _, future, latest, _ = demand.build_matrices(config)[0]
        self.assertEqual(30, config["first"]-latest["until"])
        np.testing.assert_array_equal(future[:, -1], [1., 2., 3.])
        self.assertEqual({1., 2., 3.}, set(x[:, -1]))
        result = demand.predict(data, MeanRegressor)
        self.assertEqual([30, 60, 90], [demand.timestamp(point["at"])-latest["until"] for point in result["forecasts"]])
        self.assertEqual([60, 90, 120], [demand.timestamp(point["until"])-latest["until"] for point in result["forecasts"]])

    def test_unit_and_grid_mismatches_fail(self):
        data = payload()
        data["observations"][2]["at"] = stamp(1800000061)
        with self.assertRaises(ValueError):
            demand.parse_request(data)
        data = payload()
        data["config"]["features"] = [dict(name="cpu", unit="cores", maxAgeSeconds=90)]
        for observation in data["observations"]:
            observation["features"]["cpu"] = dict(value=1., unit="millicores", sourceAt=observation["until"],
                                                   receivedAt=observation["receivedAt"])
        with self.assertRaisesRegex(ValueError, "unit"):
            demand.predict(data, MeanRegressor)

    def test_no_synthetic_future_exogenous_values(self):
        data = payload()
        data["config"]["features"] = [dict(name="cpu", unit="cores", maxAgeSeconds=90)]
        for observation in data["observations"]:
            observation["features"]["cpu"] = dict(value=1., unit="cores", sourceAt=observation["until"],
                                                   receivedAt=observation["receivedAt"])
        _, _, future, _, _ = demand.build_matrices(demand.parse_request(data))[0]
        # Every horizon reads exactly the same measured past feature window;
        # only deterministic phase and lead change across future intervals.
        np.testing.assert_array_equal(future[0, 49:85], future[1, 49:85])
        np.testing.assert_array_equal(future[1, 49:85], future[2, 49:85])


if __name__ == "__main__":
    unittest.main()
