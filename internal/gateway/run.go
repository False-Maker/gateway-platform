package gateway

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func Run(cfg Config) error {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Username: cfg.RedisUsername, Password: cfg.RedisPassword})
	defer rdb.Close()
	provider := cfg.Provider
	if provider == "" {
		provider = "apikey"
	}
	server := NewServer(eventsProducer(rdb, cfg.ProducerID), provider)
	store := snapshot.NewStore(snapshot.Loader{Redis: rdb})
	if err := store.Refresh(context.Background(), provider, "default"); err != nil {
		return err
	}
	if current, ok := store.Current(provider, "default"); ok {
		server.Chooser.ReplaceAtEpoch(current.Accounts, current.Epoch)
	}
	server.snapshotFresh = func() bool {
		_, ok := store.CurrentFresh(provider, "default")
		return ok
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := store.Refresh(context.Background(), provider, "default"); err != nil {
				continue
			}
			if current, ok := store.Current(provider, "default"); ok {
				server.Chooser.ReplaceAtEpoch(current.Accounts, current.Epoch)
			}
		}
	}()
	router := gin.New()
	router.GET("/metrics", gin.WrapH(observability.Default))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		var request ChatCompletionRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if request.Stream {
			status, err := server.HandleStreaming(c.Request.Context(), cfg.TenantID, "default", request, c.Writer)
			if err != nil && !c.Writer.Written() {
				c.JSON(status, gin.H{"error": err.Error()})
			}
			return
		}
		body, status, err := server.HandleNonStreaming(c.Request.Context(), cfg.TenantID, "default", request)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json", body)
	})
	router.POST("/v1/messages", func(c *gin.Context) {
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if _, stream, metadataErr := streamingRequestMetadata(body); metadataErr == nil && stream {
			status, streamErr := server.HandleMessagesStreaming(c.Request.Context(), cfg.TenantID, "default", body, c.Writer)
			if streamErr != nil && !c.Writer.Written() {
				c.JSON(status, gin.H{"error": streamErr.Error()})
			}
			return
		}
		response, status, err := server.HandleMessages(c.Request.Context(), cfg.TenantID, "default", body)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json", response)
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if _, stream, metadataErr := streamingRequestMetadata(body); metadataErr == nil && stream {
			status, streamErr := server.HandleResponsesStreaming(c.Request.Context(), cfg.TenantID, "default", body, c.Writer)
			if streamErr != nil && !c.Writer.Written() {
				c.JSON(status, gin.H{"error": streamErr.Error()})
			}
			return
		}
		response, status, err := server.HandleResponses(c.Request.Context(), cfg.TenantID, "default", body)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json", response)
	})
	return router.Run(cfg.ListenAddr)
}

func eventsProducer(rdb redis.UniversalClient, id string) events.Producer {
	return events.Producer{Redis: rdb, ProducerID: id}
}
