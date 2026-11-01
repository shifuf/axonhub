package orchestrator

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/lifecycle"
)

func TestStreamLifecycleSurvivesTwoHoursSimulated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		caller, cancel := context.WithCancel(context.Background())
		defer cancel()
		owner, execution := newStreamLifecycle(caller)
		defer owner.Close()
		started := time.Now()
		for range 145 {
			time.Sleep(time.Minute)
			require.NoError(t, execution.Err())
			require.NoError(t, owner.Deliver(caller, func() (bool, error) { return false, nil }))
			require.False(t, owner.DeliveryAcknowledged(), "ordinary output must not start terminal tail cleanup")
		}
		require.Greater(t, time.Since(started), 2*time.Hour)
		cancel()
		synctest.Wait()
		require.ErrorIs(t, execution.Err(), context.Canceled)
	})
}
func TestStreamLifecycleExplicitDeadlineStillApplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		caller, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		owner, execution := newStreamLifecycle(caller)
		defer owner.Close()
		time.Sleep(9 * time.Minute)
		require.NoError(t, execution.Err())
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		require.ErrorIs(t, execution.Err(), context.DeadlineExceeded)
		require.ErrorIs(t, context.Cause(execution), context.DeadlineExceeded)
		outcome := streamOutcome{}
		outcome.observeTransportError(execution.Err())
		require.Equal(t, streamTerminalFailed, outcome.finalState(), "gateway deadlines must not enter client cancellation accounting")
	})
}

func TestStreamLifecycleShutdownAfterTwoHoursSimulated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := lifecycle.New()
		owner, execution := newStreamLifecycle(manager.Attach(context.Background()))
		defer owner.Close()
		time.Sleep(2*time.Hour + time.Minute)
		require.NoError(t, execution.Err())
		cause := errors.New("test service shutdown")
		manager.Shutdown(cause)
		synctest.Wait()
		require.ErrorIs(t, context.Cause(execution), cause)
	})
}
