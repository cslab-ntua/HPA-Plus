"""Optional native XGBoost censored regression for CPU-capacity observations.

Callers explicitly configure a utilization threshold representing enforced CPU
capacity. Request utilization alone does not prove saturation. No observations
or replica recommendations are substituted into a forecast.
"""
import numpy as np


def cpu_bounds(targets, target_utilizations, threshold):
    values=np.asarray(targets,dtype=float)
    utilization=np.asarray(target_utilizations,dtype=float)
    if (values.ndim!=1 or not values.size or utilization.shape!=values.shape or
            not np.all(np.isfinite(values)) or np.any(values<0) or
            not np.all(np.isfinite(utilization)) or np.any(utilization<0) or
            type(threshold) not in (int,float) or not np.isfinite(threshold) or
            not 90<=threshold<=10000):
        raise ValueError('censored CPU regression requires nonnegative finite aligned targets and utilization')
    # A reversible +1 millicore transform represents genuine zeros in the
    # positive-label AFT objective. It does not overwrite recorded CPU values.
    lower=values+1.
    upper=np.where(utilization>=threshold,np.inf,lower)
    return lower,upper


class CPUAFTPredictor:
    def __init__(self,engine,booster):
        self.engine=engine
        self.booster=booster

    def predict(self,features):
        return np.maximum(0.,self.booster.predict(self.engine.DMatrix(features))-1.)


def fit_cpu_aft(engine,features,targets,target_utilizations,params,scale,threshold,weights=None):
    if type(scale) not in (int,float) or not np.isfinite(scale) or not .05<=scale<=2:
        raise ValueError('aftLossDistributionScale must be finite and in [0.05, 2]')
    lower,upper=cpu_bounds(targets,target_utilizations,threshold)
    data=engine.DMatrix(features,weight=weights)
    data.set_float_info('label_lower_bound',lower)
    data.set_float_info('label_upper_bound',upper)
    settings=dict(params)
    rounds=settings.pop('n_estimators')
    settings.pop('quantile_alpha',None)
    settings['nthread']=settings.pop('n_jobs',1)
    settings.update(objective='survival:aft',aft_loss_distribution='normal',
                    aft_loss_distribution_scale=scale,tree_method='hist',
                    base_score=float(np.exp(np.median(np.log(lower)))))
    booster=engine.train(settings,data,num_boost_round=rounds)
    return CPUAFTPredictor(engine,booster)
