package hook

import "context"

type resourceKey struct{}
type Resource struct{ Namespace, Name string }

// WithResource carries the controller-read HPAPlus identity to hook execution.
// It must never be derived from fields in a tuning request or hook URL.
func WithResource(ctx context.Context, namespace, name string) context.Context {
	return context.WithValue(ctx, resourceKey{}, Resource{Namespace: namespace, Name: name})
}
func ResourceFromContext(ctx context.Context) Resource {
	identity, _ := ctx.Value(resourceKey{}).(Resource)
	return identity
}
