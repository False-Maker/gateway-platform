package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/elucid/gateway-platform/internal/events"
	"github.com/elucid/gateway-platform/internal/observability"
	"github.com/elucid/gateway-platform/internal/snapshot"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const snapshotPollInterval = 5 * time.Second

// SessionKeyHeader carries the client's sticky-session handle. It is optional;
// without it every request is scheduled independently.
const SessionKeyHeader = "X-Session-Key"

func Run(cfg Config) error {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Username: cfg.RedisUsername, Password: cfg.RedisPassword})
	defer rdb.Close()
	provider := cfg.Provider
	if provider == "" {
		provider = "apikey"
	}
	server := NewServer(eventsProducer(rdb, cfg.ProducerID), provider)
	server.ChooseAllPlatforms = true
	server.Sticky = Sticky{Redis: rdb}
	ctx := context.Background()
	loader := BucketLoader{Redis: rdb, Chooser: server.Chooser}
	if err := loader.RefreshAll(ctx); err != nil {
		return err
	}
	server.snapshotFresh = func() bool { return true } // per-bucket freshness is enforced inside Chooser
	auth := NewAuthenticator(rdb)
	if err := auth.Refresh(ctx); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(snapshotPollInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := loader.RefreshAll(ctx); err != nil {
				log.Printf("gateway snapshot refresh: %v", err)
			}
			if err := auth.Refresh(ctx); err != nil {
				log.Printf("gateway token refresh: %v", err)
			}
		}
	}()
	return NewRouter(server, auth).Run(cfg.ListenAddr)
}

// BucketLoader keeps every published bucket in the chooser. Buckets that
// disappear from the registry are removed; a bucket whose load fails keeps its
// last-known-good accounts until the Redis data TTL expires them.
type BucketLoader struct {
	Redis   redis.UniversalClient
	Chooser *Chooser
}

func (l BucketLoader) RefreshAll(ctx context.Context) error {
	registered, err := snapshot.RegisteredBuckets(ctx, l.Redis)
	if err != nil {
		return err
	}
	wanted := make(map[[2]string]struct{}, len(registered))
	var loadErr error
	for _, bucket := range registered {
		wanted[[2]string{bucket.Platform, bucket.Group}] = struct{}{}
		loaded, err := (snapshot.Loader{Redis: l.Redis}).Load(ctx, bucket.Platform, bucket.Group)
		if err != nil {
			loadErr = errors.Join(loadErr, err)
			continue
		}
		l.Chooser.ReplaceBucket(bucket.Platform, bucket.Group, loaded.Accounts, loaded.Epoch, loaded.ValidUntil)
	}
	for _, installed := range l.Chooser.Buckets() {
		if _, keep := wanted[installed]; !keep {
			l.Chooser.RemoveBucket(installed[0], installed[1])
		}
	}
	return loadErr
}

// NewRouter wires the HTTP surface. Every inference route authenticates first;
// TenantID and Group come only from the verified AuthContext.
func NewRouter(server *Server, auth *Authenticator) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())
	router.GET("/metrics", gin.WrapH(observability.Default))
	authenticated := router.Group("/", func(c *gin.Context) {
		authCtx, err := auth.AuthenticateRequest(c.Request)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": ErrUnauthorized.Error()})
			return
		}
		// B4.4: refuse before any upstream work happens. Because this sits in
		// the auth middleware, a blocked tenant never reaches a handler, so no
		// upstream call is made, no Release event is produced and no
		// usage_ledger row is written. The decision is read off the snapshot
		// the authenticator already had in memory -- no PostgreSQL, no control.
		if authCtx.BillingBlocked {
			observability.Default.AddCounter("gateway_billing_rejected_total", 1, "tenant", authCtx.TenantID)
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error": ErrBillingBlocked.Error(), "tenant": authCtx.TenantID,
			})
			return
		}
		sessionKey, err := normalizeSessionKey(c.GetHeader(SessionKeyHeader))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		authCtx.SessionKey = sessionKey
		c.Set("auth", authCtx)
		c.Request = c.Request.WithContext(WithAuth(c.Request.Context(), authCtx))
		c.Next()
	})
	authenticated.POST("/v1/chat/completions", func(c *gin.Context) {
		authCtx := c.MustGet("auth").(AuthContext)
		var request ChatCompletionRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if request.Stream {
			status, err := server.HandleStreaming(c.Request.Context(), authCtx.TenantID, authCtx.Group, request, c.Writer)
			if err != nil && !c.Writer.Written() {
				c.JSON(status, gin.H{"error": err.Error()})
			}
			return
		}
		body, status, err := server.HandleNonStreaming(c.Request.Context(), authCtx.TenantID, authCtx.Group, request)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json", body)
	})
	authenticated.POST("/v1/messages", func(c *gin.Context) {
		authCtx := c.MustGet("auth").(AuthContext)
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if _, stream, metadataErr := streamingRequestMetadata(body); metadataErr == nil && stream {
			status, streamErr := server.HandleMessagesStreaming(c.Request.Context(), authCtx.TenantID, authCtx.Group, body, c.Writer)
			if streamErr != nil && !c.Writer.Written() {
				c.JSON(status, gin.H{"error": streamErr.Error()})
			}
			return
		}
		response, status, err := server.HandleMessages(c.Request.Context(), authCtx.TenantID, authCtx.Group, body)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json", response)
	})
	authenticated.POST("/v1/responses", func(c *gin.Context) {
		authCtx := c.MustGet("auth").(AuthContext)
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if _, stream, metadataErr := streamingRequestMetadata(body); metadataErr == nil && stream {
			status, streamErr := server.HandleResponsesStreaming(c.Request.Context(), authCtx.TenantID, authCtx.Group, body, c.Writer)
			if streamErr != nil && !c.Writer.Written() {
				c.JSON(status, gin.H{"error": streamErr.Error()})
			}
			return
		}
		response, status, err := server.HandleResponses(c.Request.Context(), authCtx.TenantID, authCtx.Group, body)
		if err != nil {
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Data(http.StatusOK, "application/json", response)
	})
	return router
}

func eventsProducer(rdb redis.UniversalClient, id string) events.Producer {
	return events.Producer{Redis: rdb, ProducerID: id}
}
