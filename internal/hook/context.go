package hook

import (
	"context"
	api "github.com/cslab-ntua/HPA-Plus/api/v1alpha1"
)

type ContextExecuter interface {
	ExecuteWithValueContext(context.Context, *api.HookDefinition, string) (string, error)
}

func Execute(ctx context.Context, executer Executer, definition *api.HookDefinition, value string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if executer, ok := executer.(ContextExecuter); ok {
		return executer.ExecuteWithValueContext(ctx, definition, value)
	}
	return executer.ExecuteWithValue(definition, value)
}
