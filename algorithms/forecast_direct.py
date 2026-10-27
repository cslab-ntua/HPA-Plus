"""Bounded direct lead-time features for one scalar XGBoost regressor."""
import bisect
import math
import numpy as np
from forecast_budget import training_matrix, tree_budget
from forecast_features import build_feature_row
from forecast_trends import cpu_trend_slopes, cpu_trend_row, trend_feature_header


def direct_matrices(series, metrics, lags, window, times, interval, steps,
                    training_phase=None, future_phase=None, estimators=None, depth=None,
                    return_target_indices=False, time_features=False):
    if len(times)!=len(series) or len(times)<2 or not math.isfinite(interval) or interval<=0 or any(not math.isfinite(t) for t in times) or any(b<=a for a,b in zip(times,times[1:])):
        raise ValueError('direct forecasting requires increasing finite timestamps')
    if type(steps) is not int or steps<1:
        raise ValueError('direct forecasting requires positive steps')
    if training_phase is not None and (len(training_phase)!=len(series) or future_phase is None or len(future_phase)!=steps):
        raise ValueError('direct phase dimensions do not match')
    if type(time_features) is not bool:
        raise ValueError('CPU trend options must be boolean')
    if time_features and (type(lags) is not int or type(window) is not int or not 1<=window<=lags):
        raise ValueError('CPU trends require a positive configured window within lags')
    trend_count=len(trend_feature_header(window)) if time_features else 0
    extra=1+(len(training_phase[0]) if training_phase is not None else 0)+trend_count
    upper_rows=max(0,len(series)-lags)*min(steps,len(series)-1)
    training_matrix(upper_rows+lags,lags,window,metrics is not None,extra)
    training_matrix(steps+lags,lags,window,metrics is not None,extra)
    pairs=[]
    tolerance=min(5.,interval*.2)
    for i in range(lags-1,len(series)-1):
        count=min(steps,max(0,math.floor((times[-1]-times[i]+tolerance)/interval)))
        for step in range(1,count+1):
            target=times[i]+step*interval
            k=bisect.bisect_left(times,target)
            candidates=[j for j in (k-1,k) if i<j<len(times)]
            if not candidates:continue
            j=min(candidates,key=lambda j:abs(times[j]-target))
            if abs(times[j]-target)<=tolerance:pairs.append((i,j))
    if len(pairs)<8:
        raise ValueError('insufficient clock-matched direct training targets')
    tree_budget('XGBoost',len(pairs)+lags,lags,window,metrics is not None,estimators,depth,None,extra)

    # Each anchor's trend is computed once, only from its observed feature
    # window. Future rows use the final real window, never generated CPU.
    trends={}
    if trend_count:
        for i in sorted({i for i,_ in pairs}|{len(series)-1}):
            trends[i]=cpu_trend_slopes(series[i-window+1:i+1],times[i-window+1:i+1])

    def row(i,target,phase):
        value=build_feature_row(series[i-window+1:i+1],metrics[i-window+1:i+1] if metrics is not None else None)
        if phase is not None:value.extend(phase)
        value.append((target-times[i])/interval)
        if trend_count:
            # Multiply the exact native fractional lead, matching the screened
            # representation rather than rounding to nominal history cadence.
            value.extend(cpu_trend_row(trends[i],series[i],value[-1]*interval))
        return value

    x=np.array([row(i,times[j],training_phase[j] if training_phase is not None else None) for i,j in pairs])
    y=np.array([series[j] for _,j in pairs])
    future=np.array([row(len(series)-1,times[-1]+(s+1)*interval,future_phase[s] if future_phase is not None else None) for s in range(steps)])
    if return_target_indices:
        return x,y,future,np.asarray([j for _,j in pairs],dtype=int)
    return x,y,future


def direct_forecast(model, features, aggregation='last', return_values=False):
    if aggregation not in ('last','maximum'):
        raise ValueError('forecast aggregation must be last or maximum')
    values=np.asarray(model.predict(features))
    if values.shape!=(len(features),) or len(values)==0 or not np.all(np.isfinite(values)) or np.any(values < -(2**63)) or np.any(values >= 2**63):
        raise ValueError('direct forecast must contain finite int64 CPU millicores')
    if return_values:
        return values
    return float(np.max(values) if aggregation=='maximum' else values[-1])
