package mail

import (
	"context"
	"time"
)

const toolTimeout = 25 * time.Second

func boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= toolTimeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, toolTimeout)
}
