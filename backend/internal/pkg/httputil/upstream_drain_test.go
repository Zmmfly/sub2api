package httputil

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamDrainStartsOnlyAfterClientCancellation(t *testing.T) {
	client, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstream, release := NewUpstreamAttemptContext(DetachUpstreamContext(client), 30*time.Millisecond)
	defer release()
	time.Sleep(60 * time.Millisecond)
	require.NoError(t, upstream.Err())
	cancelClient()
	select {
	case <-upstream.Done():
		t.Fatal("cancelled before late usage drain window")
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case <-upstream.Done():
		require.ErrorIs(t, upstream.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("drain exceeded total budget")
	}
}

func TestUpstreamDrainOverrideAndNestedDetachKeepOriginalSignal(t *testing.T) {
	client, cancelClient := context.WithCancel(context.Background())
	parent := DetachUpstreamContext(DetachUpstreamContext(client, 20*time.Millisecond))
	upstream, release := NewUpstreamAttemptContext(parent, time.Second)
	defer release()
	cancelClient()
	select {
	case <-upstream.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("nested detach lost original cancellation or override")
	}
}

func TestUpstreamDrainReleaseDoesNotCancelCaller(t *testing.T) {
	client, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstream, release := NewUpstreamAttemptContext(DetachUpstreamContext(client), time.Second)
	release()
	release()
	require.ErrorIs(t, upstream.Err(), context.Canceled)
	require.NoError(t, client.Err())
	cancelClient()
}

func TestUpstreamClientErrRetainsOriginalCancellation(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "nested_detached"}[detached], func(t *testing.T) {
			client, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := client
			if detached {
				ctx = DetachUpstreamContext(DetachUpstreamContext(client))
			}
			require.NoError(t, UpstreamClientErr(ctx))
			cancel()
			require.ErrorIs(t, UpstreamClientErr(ctx), context.Canceled)
			if detached {
				require.NoError(t, ctx.Err(), "billing drain remains detached")
			}
		})
	}
	require.NoError(t, UpstreamClientErr(context.TODO()))
	require.NoError(t, UpstreamClientErr(context.Background()))
}

func TestUpstreamClientErrDetectsDeadlineAndAttemptCancellation(t *testing.T) {
	client, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	detached := DetachUpstreamContext(client)
	require.ErrorIs(t, UpstreamClientErr(detached), context.DeadlineExceeded)
	require.NoError(t, detached.Err())

	upstream, release := NewUpstreamAttemptContext(DetachUpstreamContext(context.Background()), time.Second)
	release()
	require.ErrorIs(t, UpstreamClientErr(upstream), context.Canceled)
}

func TestUpstreamClientErrDoesNotEndExistingDrain(t *testing.T) {
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, release := NewUpstreamAttemptContext(DetachUpstreamContext(client), time.Second)
	defer release()
	cancel()
	require.ErrorIs(t, UpstreamClientErr(upstream), context.Canceled)
	require.NoError(t, upstream.Err(), "the in-flight attempt can still collect usage")
}
