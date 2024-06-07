package network

import (
	"context"
	"testing"

	"github.com/docker/cli/internal/test"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/pkg/errors"
)

func TestNetworkPrunePromptTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cli := test.NewFakeCli(&fakeClient{
		networkPruneFunc: func(ctx context.Context, pruneFilters filters.Args) (network.PruneReport, error) {
			return network.PruneReport{}, errors.New("fakeClient networkPruneFunc should not be called")
		},
	})
	cmd := NewPruneCommand(cli)
	test.TerminatePrompt(ctx, t, cmd, cli)
}
