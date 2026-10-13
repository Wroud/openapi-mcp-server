package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

type schemaCache struct {
	lock     sync.Mutex
	loader   *SchemaLoader
	ttl      time.Duration
	document *openapi3.T
	expires  time.Time
}

func (cache *schemaCache) load(ctx context.Context) (*openapi3.T, error) {
	cache.lock.Lock()
	defer cache.lock.Unlock()
	if cache.document != nil && time.Now().Before(cache.expires) {
		return cache.document, nil
	}
	document, degraded, err := cache.loader.load(ctx, http.Header{})
	if err == nil && !degraded {
		cache.document, cache.expires = document, time.Now().Add(cache.ttl)
		return document, nil
	}
	if cache.document != nil {
		slog.WarnContext(ctx, "Serving the last complete API schema", "degraded", degraded, "error", err)
		return cache.document, nil
	}
	return document, err
}
