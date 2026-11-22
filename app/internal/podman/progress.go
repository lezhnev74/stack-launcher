package podman

import (
	"context"
	"fmt"
)

type launchProgressKey struct{}

// WithLaunchProgress attaches a reporter for startup milestones. Messages omit
// command output, launch command arguments, and environment values.
func WithLaunchProgress(ctx context.Context, report func(string)) context.Context {
	return context.WithValue(ctx, launchProgressKey{}, report)
}

func reportProgress(ctx context.Context, format string, args ...any) {
	if report, _ := ctx.Value(launchProgressKey{}).(func(string)); report != nil {
		report(fmt.Sprintf(format, args...))
	}
}
