package api

import (
	"compress/gzip"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// accessLog writes one structured line per REST request to the log. It must
// wrap compress() so "bytes" is the compressed size actually sent.
func accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := c.Writer.Status()
		level := slog.LevelInfo
		if c.Request.URL.Path == "/api/health" {
			level = slog.LevelDebug // platform health checks would flood the log
		}
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}
		slog.Log(c.Request.Context(), level, "http request",
			"method", c.Request.Method, "path", c.Request.URL.Path, "query", c.Request.URL.RawQuery,
			"status", status, "bytes", c.Writer.Size(), "duration_ms", float64(time.Since(start).Microseconds())/1000,
			"remote", c.ClientIP())
	}
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }}

type gzipWriter struct {
	gin.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipWriter) Write(b []byte) (int, error)       { return g.gz.Write(b) }
func (g *gzipWriter) WriteString(s string) (int, error) { return g.gz.Write([]byte(s)) }

// compress gzips REST responses for clients that accept it. The 3-day candle
// history is ~330 KB of JSON and compresses about 4x.
func compress() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		gz := gzPool.Get().(*gzip.Writer)
		gz.Reset(c.Writer)
		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		c.Writer = &gzipWriter{ResponseWriter: c.Writer, gz: gz}
		defer func() {
			gz.Close() // flush before the handler chain returns
			gzPool.Put(gz)
		}()
		c.Next()
	}
}
