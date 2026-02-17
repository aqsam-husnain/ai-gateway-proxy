// Package middleware provides Gin middleware for CORS, authentication, and
// structured request logging.
package middleware

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CORS returns a Gin handler that sets permissive CORS headers and handles
// pre-flight OPTIONS requests with a 204 No Content response.
//
// The behaviour mirrors the JS proxy: any origin is allowed, and the full set
// of headers used by OpenAI / Claude / Gemini clients is explicitly permitted.
func CORS() gin.HandlerFunc {
	allowedMethods := strings.Join([]string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
		http.MethodHead,
	}, ", ")

	allowedHeaders := strings.Join([]string{
		"Authorization",
		"Content-Type",
		"Accept",
		"Origin",
		"X-Requested-With",
		"X-Goog-Api-Key",
		"X-Goog-User-Project",
		"Anthropic-Version",
		"Anthropic-Beta",
		"X-Api-Key",
		"Cache-Control",
		"Pragma",
	}, ", ")

	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", allowedMethods)
		c.Header("Access-Control-Allow-Headers", allowedHeaders)
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// Auth returns a Gin handler that validates the caller's API key.
//
// The key is accepted from any of three locations (in order of precedence):
//  1. Authorization: Bearer <key>  header
//  2. x-goog-api-key              header
//  3. ?key=<key>                  query parameter
//
// If requiredAPIKey is empty the middleware is a no-op (auth is disabled).
// An incorrect or missing key yields 401 Unauthorized with a JSON error body.
func Auth(requiredAPIKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Auth disabled – skip.
		if requiredAPIKey == "" {
			c.Next()
			return
		}

		// 1. Authorization: Bearer
		var providedKey string
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			providedKey = strings.TrimPrefix(authHeader, "Bearer ")
		}

		// 2. x-goog-api-key header (Gemini clients use this)
		if providedKey == "" {
			providedKey = c.GetHeader("x-goog-api-key")
		}

		// 3. ?key= query parameter
		if providedKey == "" {
			providedKey = c.Query("key")
		}

		if providedKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "Missing API key. Provide it via 'Authorization: Bearer <key>', 'x-goog-api-key' header, or '?key=' query parameter.",
					"type":    "authentication_error",
					"code":    "missing_api_key",
				},
			})
			return
		}

		if providedKey != requiredAPIKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{
					"message": "Invalid API key.",
					"type":    "authentication_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}

		c.Next()
	}
}

// Logger returns a Gin handler that logs each request's method, path, status
// code, client IP, and elapsed time using the standard library logger.
//
// Logging is skipped for the /health endpoint to reduce noise in production
// monitoring.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method

		c.Next()

		// Skip noisy health-check polling.
		if path == "/health" {
			return
		}

		duration := time.Since(start)
		status := c.Writer.Status()
		clientIP := c.ClientIP()

		log.Printf("[%s] %s %s %d %s %s",
			method,
			clientIP,
			path,
			status,
			duration.String(),
			c.Errors.ByType(gin.ErrorTypePrivate).String(),
		)
	}
}
