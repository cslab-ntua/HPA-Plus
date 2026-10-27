package algorithm

import "context"

// Runner retains the original adapter interface for callers and test doubles.
type Runner interface {
	RunAlgorithmWithValue(string, string, int) (string, error)
}

type ContextRunner interface {
	RunAlgorithmWithValueContext(context.Context, string, string, int) (string, error)
}

func Run(ctx context.Context, runner Runner, script, payload string, timeout int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if runner, ok := runner.(ContextRunner); ok {
		return runner.RunAlgorithmWithValueContext(ctx, script, payload, timeout)
	}
	return runner.RunAlgorithmWithValue(script, payload, timeout)
}

type SessionRunner interface {
	RunSessionWithValue(string, string, string, int) (string, error)
}

type ContextSessionRunner interface {
	RunSessionWithValueContext(context.Context, string, string, string, int) (string, error)
}

func RunSession(ctx context.Context, runner SessionRunner, id, script, payload string, timeout int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if runner, ok := runner.(ContextSessionRunner); ok {
		return runner.RunSessionWithValueContext(ctx, id, script, payload, timeout)
	}
	return runner.RunSessionWithValue(id, script, payload, timeout)
}
