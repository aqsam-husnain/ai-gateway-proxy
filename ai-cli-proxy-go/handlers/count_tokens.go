package handlers

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// HandleCountTokens handles POST /v1/messages/count_tokens (and any path
// containing /count_tokens).
//
// It reads the raw request body and returns a rough token estimate based on
// byte length divided by 4, which approximates the average number of bytes per
// token for most LLM tokenizers.
func HandleCountTokens() gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}

		// Rough estimate: ~4 bytes per token on average.
		inputTokens := len(body) / 4
		if inputTokens < 1 && len(body) > 0 {
			inputTokens = 1
		}

		c.JSON(http.StatusOK, gin.H{
			"input_tokens": inputTokens,
		})
	}
}
