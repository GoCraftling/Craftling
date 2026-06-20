package middleware

import "github.com/gin-gonic/gin"

// AgentAuth guards the agent-facing endpoints (/api/v1/agent/*).
//
// PLACEHOLDER: agents are not yet authenticated. P10 replaces this with
// per-host tokens or mTLS with rotation. It exists now as the seam those
// credentials will plug into, so routing and handlers don't move later.
func AgentAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}
