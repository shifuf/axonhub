package middleware

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWithTimeout_ZeroPreservesCallerContext(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second} {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		WithTimeout(duration)(c)
		require.Same(t, ctx, c.Request.Context())
		require.NoError(t, c.Request.Context().Err())
		cancel()
		require.ErrorIs(t, c.Request.Context().Err(), context.Canceled)
	}
}
func TestWithTimeout_PositiveRetainsShorterParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	WithTimeout(time.Hour)(c)
	want, _ := ctx.Deadline()
	got, ok := c.Request.Context().Deadline()
	require.True(t, ok)
	require.Equal(t, want, got)
}
func TestWithTimeout_PositiveAppliesConfiguredDeadline(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	before := time.Now()
	WithTimeout(2 * time.Second)(c)
	got, ok := c.Request.Context().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, before.Add(2*time.Second), got, time.Second)
}

func TestWithTimeout_ZeroClearsServerWriteDeadline(t *testing.T) {
	router := gin.New()
	router.GET("/", WithTimeout(0), func(c *gin.Context) { time.Sleep(40 * time.Millisecond); c.String(http.StatusOK, "upstream completed") })
	server := httptest.NewUnstartedServer(router)
	server.Config.WriteTimeout = 5 * time.Millisecond
	server.Start()
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(server.URL)
	require.NoError(t, err, "zero route timeout must also override the server socket write deadline")
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
}
