package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/log"
)

func WithTimeout(ts time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Zero disables the gateway deadline, not the caller's cancellation.
		if ts <= 0 {
			// The HTTP server also installs a socket write deadline shared by
			// all routes. Disabling only the context timer would still cut off
			// a slow upstream response at that hidden transport deadline.
			err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
			if err != nil && !errors.Is(err, http.ErrNotSupported) {
				log.Warn(c.Request.Context(), "Failed to clear unlimited request write deadline", log.Cause(err))
			}
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), ts)
		defer cancel()

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
