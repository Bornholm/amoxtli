// Package stagectx carries, in a request context, the set of optional search
// stages allowed to run for that request. It lets a caller pick a retrieval
// profile per search while the stages themselves are configured once.
package stagectx

import "context"

type key struct{}

// WithAllowed returns a context in which only the given stages may run. A
// context without such a set allows every configured stage.
func WithAllowed(ctx context.Context, stages ...string) context.Context {
	allowed := make(map[string]bool, len(stages))
	for _, s := range stages {
		allowed[s] = true
	}
	return context.WithValue(ctx, key{}, allowed)
}

// Skipped reports whether the stage must not run for the request carried by
// ctx.
func Skipped(ctx context.Context, stage string) bool {
	allowed, ok := ctx.Value(key{}).(map[string]bool)
	if !ok {
		return false
	}
	return !allowed[stage]
}
