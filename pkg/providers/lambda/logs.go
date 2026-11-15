package lambda

import (
	"context"
	"fmt"
	"github.com/aibrix/neocloud-node/pkg/core"
	"io"
)

var _ core.ProviderLogReader = (*Client)(nil)

func (c *Client) OpenLogs(ctx context.Context, t core.LogTarget, o core.LogOptions) (io.ReadCloser, error) {
	if c.logStore == nil || c.runnerLogsURL == "" {
		return nil, fmt.Errorf("Lambda runner logs are not configured")
	}
	return c.logStore.OpenLogs(ctx, t, o)
}
