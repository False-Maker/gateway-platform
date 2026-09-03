package control

import (
	"time"

	"github.com/elucid/gateway-platform/internal/events"
	"github.com/redis/go-redis/v9"
)

func eventsConsumer(rdb redis.UniversalClient, consumer string, ledger Ledger) events.Consumer {
	return events.Consumer{Redis: rdb, Consumer: consumer, Handler: ledger, Now: time.Now}
}

var _ events.Handler = Ledger{}
